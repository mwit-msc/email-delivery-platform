FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
ARG VERSION=development
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o /edp ./cmd/edp

FROM alpine:3.23
RUN apk add --no-cache ca-certificates tzdata && addgroup -g 10001 edp && adduser -D -u 10001 -G edp edp
COPY --from=build /edp /usr/local/bin/edp
COPY LICENSE /usr/share/licenses/edp/LICENSE
COPY third_party /usr/share/licenses/edp
COPY internal/edp/web/assets/*-OFL.txt /usr/share/licenses/edp/
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["edp"]
CMD ["serve"]
