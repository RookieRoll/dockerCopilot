FROM alpine

LABEL authors="onlyLTY" \
      org.opencontainers.image.title="Docker Copilot" \
      org.opencontainers.image.ref.name="2807645688/dockercopilot"

WORKDIR /app

# BuildKit provides TARGETARCH for each platform in a multi-platform build.
ARG TARGETARCH

COPY dc-back/dist/linux/${TARGETARCH}/dockerCopilot /app/dockerCopilot
COPY dc-back/etc/. /app/etc/
COPY start.sh /app/start.sh

RUN apk add --no-cache tzdata \
    && chmod +x /app/start.sh /app/dockerCopilot

ENV WORKDIR="/app" \
    secretKey="" \
    DOCKER_HOST="unix:///var/run/docker.sock" \
    BACKUP_DIR="/data/backups" \
    TZ="Asia/Shanghai"

VOLUME ["/data"]
EXPOSE 12712

CMD ["/app/start.sh"]
