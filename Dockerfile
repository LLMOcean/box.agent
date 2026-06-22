FROM golang:1.22-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /bin/box-agent .

FROM alpine:3.18

RUN apk add --no-cache ca-certificates

COPY --from=build /bin/box-agent /bin/box-agent

ENTRYPOINT ["/bin/box-agent"]
