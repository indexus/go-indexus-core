# Indexus node image for spawned workers (ECR).
FROM golang:1.22.5-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/node ./app/node

FROM amazonlinux:2023
RUN dnf install -y awscli ca-certificates && dnf clean all
COPY --from=build /out/node /opt/indexus/node
RUN mkdir -p /var/lib/indexus /etc/indexus
ENV HOME=/var/lib/indexus
WORKDIR /var/lib/indexus
ENTRYPOINT ["/opt/indexus/node"]
