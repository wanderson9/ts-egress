FROM --platform=$BUILDPLATFORM node:25-alpine AS frontend
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend /src/internal/ui/dist ./internal/ui/dist
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /ts-egress ./cmd/ts-egress

FROM alpine:3.23
RUN apk add --no-cache ca-certificates && addgroup -g 10001 egress && adduser -D -u 10001 -G egress egress && mkdir /data && chown egress:egress /data
COPY --from=build /ts-egress /usr/local/bin/ts-egress
USER 10001:10001
VOLUME ["/data"]
EXPOSE 8080 80 443
ENTRYPOINT ["ts-egress"]
CMD ["-state-dir", "/data", "-setup", "0.0.0.0:8080"]
