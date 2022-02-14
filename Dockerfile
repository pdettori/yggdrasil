FROM golang:1.16 as builder
WORKDIR /workspace

COPY . .

RUN go mod download
RUN	mkdir -p ./bin
RUN go build -o ./bin ./cmd/yggd

FROM gcr.io/distroless/static:nonroot
ENV USER_UID=10001
WORKDIR /
COPY --from=builder /workspace/bin/yggd /yggd

USER ${USER_UID}

ENTRYPOINT ["/yggd"]