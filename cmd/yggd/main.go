package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"git.sr.ht/~spc/go-log"
	"github.com/google/uuid"
	"github.com/redhatinsights/yggdrasil"
	internal "github.com/redhatinsights/yggdrasil/internal"
	http2 "github.com/redhatinsights/yggdrasil/internal/clients/http"
	"github.com/redhatinsights/yggdrasil/internal/transport"
	"github.com/redhatinsights/yggdrasil/internal/transport/http"
	"github.com/redhatinsights/yggdrasil/internal/transport/mqtt"
	pb "github.com/redhatinsights/yggdrasil/protocol"
	"github.com/rjeczalik/notify"
	"github.com/urfave/cli/v2"
	"github.com/urfave/cli/v2/altsrc"
	"google.golang.org/grpc"
)

var ClientID = ""

type TransportType string
type ClientIDSource string

const (
	MQTT TransportType = "mqtt"
	HTTP TransportType = "http"

	CertCN    ClientIDSource = "cert-cn"
	MachineID ClientIDSource = "machine-id"
)

func main() {
	app := cli.NewApp()
	app.Name = yggdrasil.ShortName + "d"
	app.Version = yggdrasil.Version
	app.Usage = "connect the system to " + yggdrasil.Provider

	defaultConfigFilePath, err := yggdrasil.ConfigPath()
	if err != nil {
		log.Fatal(err)
	}

	app.Flags = []cli.Flag{
		&cli.StringFlag{
			Name:      "config",
			Value:     defaultConfigFilePath,
			TakesFile: true,
			Usage:     "Read config values from `FILE`",
		},
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:  "log-level",
			Value: "info",
			Usage: "Set the logging output level to `LEVEL`",
		}),
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:  "cert-file",
			Usage: "Use `FILE` as the client certificate",
		}),
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:  "key-file",
			Usage: "Use `FILE` as the client's private key",
		}),
		altsrc.NewStringSliceFlag(&cli.StringSliceFlag{
			Name:   "ca-root",
			Hidden: true,
			Usage:  "Use `FILE` as the root CA",
		}),
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:   "topic-prefix",
			Value:  yggdrasil.TopicPrefix,
			Hidden: true,
			Usage:  "Use `PREFIX` as the MQTT topic prefix",
		}),
		altsrc.NewStringSliceFlag(&cli.StringSliceFlag{
			Name:  "broker",
			Usage: "Connect to the broker specified in `URI`",
		}),
		&cli.BoolFlag{
			Name:   "generate-man-page",
			Hidden: true,
		},
		&cli.BoolFlag{
			Name:   "generate-markdown",
			Hidden: true,
		},
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:  "data-host",
			Usage: "Force all HTTP traffic over `HOST`",
			Value: yggdrasil.DataHost,
		}),
		&cli.StringFlag{
			Name:   "socket-addr",
			Usage:  "Force yggd to listen on `SOCKET`",
			Value:  fmt.Sprintf("@yggd-dispatcher-%v", randomString(6)),
			Hidden: true,
		},
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:   "transport",
			Usage:  "Force yggdrasil to use specific transport",
			Value:  string(MQTT),
			Hidden: true,
		}),
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:   "http-server",
			Usage:  "HTTP server to use for HTTP transport",
			Value:  "localhost:8888",
			Hidden: true,
		}),
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:   "client-id-source",
			Usage:  "Source of the client-id used to connect to remote servers. Possible values: cert-cn, machine-id",
			Value:  "cert-cn",
			Hidden: true,
		}),
	}

	// This BeforeFunc will load flag values from a config file only if the
	// "config" flag value is non-zero.
	app.Before = func(c *cli.Context) error {
		filePath := c.String("config")
		if filePath != "" {
			inputSource, err := altsrc.NewTomlSourceFromFile(filePath)
			if err != nil {
				return err
			}
			return altsrc.ApplyInputSourceValues(c, inputSource, app.Flags)
		}
		return nil
	}

	app.Action = func(c *cli.Context) error {
		if c.Bool("generate-man-page") || c.Bool("generate-markdown") {
			type GenerationFunc func() (string, error)
			var generationFunc GenerationFunc
			if c.Bool("generate-man-page") {
				generationFunc = c.App.ToMan
			} else if c.Bool("generate-markdown") {
				generationFunc = c.App.ToMarkdown
			}
			data, err := generationFunc()
			if err != nil {
				return err
			}
			fmt.Println(data)
			return nil
		}

		// Set TopicPrefix globally if the config option is non-zero
		if c.String("topic-prefix") != "" {
			yggdrasil.TopicPrefix = c.String("topic-prefix")
		}

		// Set DataHost globally if the config option is non-zero
		if c.String("data-host") != "" {
			yggdrasil.DataHost = c.String("data-host")
		}

		// Set up a channel to receive the TERM or INT signal over and clean up
		// before quitting.
		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)

		// Set up logging
		level, err := log.ParseLevel(c.String("log-level"))
		if err != nil {
			return cli.Exit(err, 1)
		}
		log.SetLevel(level)
		log.SetPrefix(fmt.Sprintf("[%v] ", app.Name))
		if log.CurrentLevel() >= log.LevelDebug {
			log.SetFlags(log.LstdFlags | log.Llongfile)
		}

		log.Infof("starting %v version %v", app.Name, app.Version)

		log.Trace("attempting to kill any orphaned workers")
		if err := killWorkers(); err != nil {
			return cli.Exit(fmt.Errorf("cannot kill workers: %w", err), 1)
		}

		ClientID, err = getClientID(c)
		if err != nil {
			return cli.Exit(err, 1)
		}

		// Read certificates, create a TLS config, and initialize HTTP client
		var certData, keyData []byte
		if c.String("cert-file") != "" && c.String("key-file") != "" {
			var err error
			certData, err = ioutil.ReadFile(c.String("cert-file"))
			if err != nil {
				return cli.Exit(fmt.Errorf("cannot read certificate file: %v", err), 1)
			}
			keyData, err = ioutil.ReadFile(c.String("key-file"))
			if err != nil {
				return cli.Exit(fmt.Errorf("cannot read key file: %w", err), 1)
			}
		}
		rootCAs := make([][]byte, 0)
		for _, file := range c.StringSlice("ca-root") {
			data, err := ioutil.ReadFile(file)
			if err != nil {
				return cli.Exit(fmt.Errorf("cannot read certificate authority: %v", err), 1)
			}
			rootCAs = append(rootCAs, data)
		}
		tlsConfig, err := newTLSConfig(certData, keyData, rootCAs)
		if err != nil {
			return cli.Exit(fmt.Errorf("cannot create TLS config: %w", err), 1)
		}
		httpClient := http2.NewHTTPClient(tlsConfig, getUserAgent(app))

		// Create gRPC dispatcher service
		d := newDispatcher(httpClient)
		s := grpc.NewServer()
		pb.RegisterDispatcherServer(s, d)

		l, err := net.Listen("unix", c.String("socket-addr"))
		if err != nil {
			return cli.Exit(fmt.Errorf("cannot listen to socket: %w", err), 1)
		}
		go func() {
			log.Infof("listening on socket: %v", c.String("socket-addr"))
			if err := s.Serve(l); err != nil {
				log.Errorf("cannot start server: %v", err)
			}
		}()

		controlPlaneTransport, err := createTransport(c, tlsConfig, d)
		if err != nil {
			return cli.Exit(err.Error(), 1)
		}
		err = controlPlaneTransport.Start()
		if err != nil {
			return cli.Exit(err, 1)
		}

		// Start a goroutine that receives values on the 'dispatchers' channel
		// and publishes "connection-status" messages to MQTT.
		var prevDispatchersHash atomic.Value
		go func() {
			for dispatchers := range d.dispatchers {
				data, err := json.Marshal(dispatchers)
				if err != nil {
					log.Errorf("cannot marshal dispatcher map to JSON: %v", err)
					continue
				}

				// Create a checksum of the dispatchers map. If it's identical
				// to the previous checksum, skip publishing a connection-status
				// message.
				sum := fmt.Sprintf("%x", sha256.Sum256(data))
				oldSum := prevDispatchersHash.Load()
				if oldSum != nil {
					if sum == oldSum.(string) {
						continue
					}
				}
				prevDispatchersHash.Store(sum)
				go transport.PublishConnectionStatus(controlPlaneTransport, dispatchers)
			}
		}()

		// Start a goroutine that receives yggdrasil.Data values on a 'send'
		// channel and dispatches them to worker processes.
		go d.sendData()

		// Start a goroutine that receives yggdrasil.Data values on a 'recv'
		// channel and publish them to MQTT.
		go transport.PublishReceivedData(controlPlaneTransport, d.recvQ)

		// Locate and start worker child processes.
		workerPath := filepath.Join(yggdrasil.LibexecDir, yggdrasil.LongName)
		if err := os.MkdirAll(workerPath, 0755); err != nil {
			return cli.Exit(fmt.Errorf("cannot create directory: %w", err), 1)
		}

		fileInfos, err := ioutil.ReadDir(workerPath)
		if err != nil {
			return cli.Exit(fmt.Errorf("cannot read contents of directory: %w", err), 1)
		}
		configDir := filepath.Join(yggdrasil.SysconfDir, yggdrasil.LongName)
		env := []string{
			"YGG_SOCKET_ADDR=unix:" + c.String("socket-addr"),
			"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
			"BASE_CONFIG_DIR=" + configDir,
			"LOG_LEVEL=" + level.String(),
			"DEVICE_ID=" + ClientID,
		}
		for _, info := range fileInfos {
			if strings.HasSuffix(info.Name(), "worker") {
				log.Debugf("starting worker: %v", info.Name())
				go startProcess(filepath.Join(workerPath, info.Name()), env, 0, d.deadWorkers)
			}
		}
		// Start a goroutine that watches the worker directory for added or
		// deleted files. Any "worker" files it detects are started up.
		go watchWorkerDir(workerPath, env, d.deadWorkers)

		// Start a goroutine that receives handler values on a channel and
		// removes the worker registration entry.
		go d.unregisterWorker()

		// Start a goroutine that watches the tags file for write events and
		// publishes connection status messages when the file changes.
		go func() {
			c := make(chan notify.EventInfo, 1)

			fp := filepath.Join(yggdrasil.SysconfDir, yggdrasil.LongName, "tags.toml")

			if err := notify.Watch(fp, c, notify.InCloseWrite, notify.InDelete); err != nil {
				log.Infof("cannot start watching '%v': %v", fp, err)
				return
			}
			defer notify.Stop(c)

			for e := range c {
				log.Debugf("received inotify event %v", e.Event())
				switch e.Event() {
				case notify.InCloseWrite, notify.InDelete:
					go transport.PublishConnectionStatus(controlPlaneTransport, d.makeDispatchersMap())
				}
			}
		}()

		<-quit

		if err := killWorkers(); err != nil {
			return cli.Exit(fmt.Errorf("cannot kill workers: %w", err), 1)
		}

		return nil
	}
	app.EnableBashCompletion = true
	app.BashComplete = internal.BashComplete

	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}

func getUserAgent(app *cli.App) string {
	return fmt.Sprintf("%v/%v", app.Name, app.Version)
}

func createTransport(c *cli.Context, tlsConfig *tls.Config, d *dispatcher) (transport.Transport, error) {
	dataHandler := createDataHandler(d)
	controlMessageHandler := createControlMessageHandler(d)

	transportType := TransportType(c.String("transport"))
	switch transportType {
	case MQTT:
		brokers := c.StringSlice("broker")
		return mqtt.NewMQTTTransport(ClientID, brokers, tlsConfig, controlMessageHandler, dataHandler)
	case HTTP:
		server := c.String("http-server")
		return http.NewHTTPTransport(ClientID, server, tlsConfig, getUserAgent(c.App), time.Second*5, controlMessageHandler, dataHandler)
	default:
		return nil, fmt.Errorf("unrecognized transport type: %v", transportType)
	}
}

func createControlMessageHandler(d *dispatcher) func(msg []byte, t transport.Transport) {
	return func(msg []byte, t transport.Transport) {
		var cmd yggdrasil.Command
		if err := json.Unmarshal(msg, &cmd); err != nil {
			log.Errorf("cannot unmarshal control message: %v", err)
			return
		}

		log.Debugf("received message %v", cmd.MessageID)
		log.Tracef("command: %+v", cmd)
		log.Tracef("Control message: %v", cmd)

		switch cmd.Content.Command {
		case yggdrasil.CommandNamePing:
			event := yggdrasil.Event{
				Type:       yggdrasil.MessageTypeEvent,
				MessageID:  uuid.New().String(),
				ResponseTo: cmd.MessageID,
				Version:    1,
				Sent:       time.Now(),
				Content:    string(yggdrasil.EventNamePong),
			}

			err := t.SendControl(event)
			if err != nil {
				log.Error(err)
			}
		case yggdrasil.CommandNameDisconnect:
			log.Info("disconnecting...")
			for _, w := range d.workers {
				disconnectWorker(w)
			}
			t.Disconnect(500)

		case yggdrasil.CommandNameReconnect:
			log.Info("reconnecting...")
			t.Disconnect(500)
			delay, err := strconv.ParseInt(cmd.Content.Arguments["delay"], 10, 64)
			if err != nil {
				log.Errorf("cannot parse data to int: %v", err)
				return
			}
			time.Sleep(time.Duration(delay) * time.Second)

			if err := t.Start(); err != nil {
				log.Errorf("cannot reconnect to broker: %v", err)
				return
			}
		default:
			log.Warnf("unknown command: %v", cmd.Content.Command)
		}
	}

}

func disconnectWorker(w worker) bool {
	conn, err := grpc.Dial("unix:"+w.addr, grpc.WithInsecure())
	if err != nil {
		log.Errorf("cannot dial socket: %v", err)
		return true
	}
	defer conn.Close()

	workerClient := pb.NewWorkerClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	_, err = workerClient.Disconnect(ctx, &pb.Empty{})
	if err != nil {
		log.Errorf("cannot disconnect worker %v", err)
	}
	return false
}

func createDataHandler(d *dispatcher) func(msg []byte) {
	return func(msg []byte) {
		var data yggdrasil.Data
		if err := json.Unmarshal(msg, &data); err != nil {
			log.Errorf("cannot unmarshal data message: %v", err)
			return
		}
		log.Tracef("message: %+v", data)
		d.sendQ <- data
	}
}

func getClientID(c *cli.Context) (string, error) {
	source := ClientIDSource(c.String("client-id-source"))
	switch source {
	case CertCN:
		return getCertID(c)
	case MachineID:
		facts, err := yggdrasil.GetCanonicalFacts()
		if err != nil {
			return "", err
		}
		return facts.MachineID, nil
	default:
		return "", fmt.Errorf("unsupported client ID source: %v", source)
	}
}

func getCertID(c *cli.Context) (string, error) {
	clientIDFile := filepath.Join(yggdrasil.LocalstateDir, yggdrasil.LongName, "client-id")
	if c.String("cert-file") != "" {
		CN, err := parseCertCN(c.String("cert-file"))
		if err != nil {
			return "", fmt.Errorf("cannot parse certificate: %w", err)
		}
		if err := setClientID([]byte(CN), clientIDFile); err != nil {
			return "", fmt.Errorf("cannot set client-id to CN: %w", err)
		}
	}

	if _, err := os.Stat(clientIDFile); os.IsNotExist(err) {
		return "", nil
	}
	clientID, err := ioutil.ReadFile(clientIDFile)
	if err != nil {
		return "", fmt.Errorf("cannot read file: %w", err)
	}
	if err != nil {
		return "", fmt.Errorf("cannot get client-id: %w", err)
	}

	if len(clientID) == 0 {
		data, err := createClientID(clientIDFile)
		if err != nil {
			return "", fmt.Errorf("cannot create client-id: %w", err)
		}
		clientID = data
	}
	return string(clientID), nil
}
