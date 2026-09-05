FROM node:24-alpine AS ui-build
WORKDIR /src/ui
COPY ui/package.json ui/package-lock.json ./
RUN npm ci
COPY ui/ ./
RUN npm run build

FROM golang:1.27.1-alpine AS build
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN mkdir -p /out /var/lib/reddotrelay && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILD_DATE}" -o /out/reddotrelay ./cmd/reddotrelay

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.licenses="AGPL-3.0-only"
COPY --from=build --chown=nonroot:nonroot /out/reddotrelay /reddotrelay
COPY --from=ui-build --chown=nonroot:nonroot /src/ui/dist /ui
COPY LICENSE NOTICE THIRD_PARTY_NOTICES.md /licenses/
COPY LICENSES /licenses/third-party/
COPY config.example.yaml /etc/reddotrelay/config.yaml
COPY --from=build --chown=nonroot:nonroot /var/lib/reddotrelay /var/lib/reddotrelay
USER nonroot:nonroot
EXPOSE 8080
VOLUME ["/var/lib/reddotrelay"]
ENTRYPOINT ["/reddotrelay"]
CMD ["-config", "/etc/reddotrelay/config.yaml"]
