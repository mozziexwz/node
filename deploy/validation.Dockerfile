# Test image consumes only explicit source-built artifacts, not a user workspace.
FROM debian:bookworm-slim
RUN apt-get update && apt-get install --no-install-recommends -y ca-certificates curl python3 && rm -rf /var/lib/apt/lists/* && useradd --system --uid 10001 --create-home msboost && mkdir -p /app/data && chown 10001:10001 /app/data
WORKDIR /app
COPY bin/msboost-server bin/msboost-agent bin/msboost-restore /usr/local/bin/
COPY apps/web/dist/ /app/apps/web/dist/
COPY --chmod=0644 installers/node/msboost.sh /app/installers/node/msboost.sh
COPY --chmod=0644 installers/reinstall/reinstall.sh installers/reinstall/manifest.json /app/installers/reinstall/
RUN chmod -R a+rX /app/apps/web/dist /app/installers
USER 10001:10001
ENV DATA_DIR=/app/data WEB_DIR=/app/apps/web/dist
ENTRYPOINT ["/usr/local/bin/msboost-server"]
