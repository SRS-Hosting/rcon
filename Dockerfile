FROM alpine:3.22

COPY rcon /rcon

USER 65535:65534

ENTRYPOINT ["/rcon"]
