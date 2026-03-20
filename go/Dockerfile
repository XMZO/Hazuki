# syntax=docker/dockerfile:1.7

FROM gcr.io/distroless/static-debian12

WORKDIR /app

COPY --chmod=755 dist/hazuki-go /app/hazuki-go

ENV HAZUKI_DB_PATH=/data/hazuki.db

EXPOSE 3100 3000 3001 3002 3200 3201

CMD ["/app/hazuki-go"]
