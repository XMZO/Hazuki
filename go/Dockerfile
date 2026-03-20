FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
  && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY dist/hazuki-go /app/hazuki-go
RUN chmod +x /app/hazuki-go

ENV HAZUKI_DB_PATH=/data/hazuki.db

EXPOSE 3100 3000 3001 3002 3200 3201

CMD ["/app/hazuki-go"]
