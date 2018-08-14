FROM alpine:latest

ENTRYPOINT [ "/bin/sh", "-c", " nc -l 9092"]

