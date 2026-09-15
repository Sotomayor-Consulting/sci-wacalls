# syntax=docker/dockerfile:1

# ---------- 1. Cliente React (UI de pareo/gestión) ----------
FROM node:22-alpine AS client
WORKDIR /app/client
COPY client/package*.json ./
RUN npm ci
COPY client/ ./
RUN npm run build

# ---------- 2. Servidor Go (binario estático, sin cgo) ----------
# El códec MLow es puro-Go y la grabación usa WAV puro-Go: no hace falta cgo,
# compilar opus ni ffmpeg. `CGO_ENABLED=0` produce un binario autocontenido.
FROM golang:1.26-alpine AS server
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /wacalls ./cmd/server

# ---------- 3. Runtime mínimo ----------
FROM alpine:3.20
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 wacalls
COPY --from=server /wacalls /usr/local/bin/wacalls
COPY --from=client /app/client/dist /app/client/dist
# La sesión de WhatsApp y la config de Chatwoot viven en SQLite bajo /data.
RUN mkdir -p /data && chown wacalls /data
VOLUME /data
USER wacalls
EXPOSE 8080 50000/udp
ENTRYPOINT ["wacalls"]
CMD ["-addr", ":8080", "-static", "/app/client/dist", "-db", "/data/wacalls.db"]
