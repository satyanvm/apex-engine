FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/apex-engine ./cmd/apex-engine \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/demo-api ./cmd/demo-api

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/apex-engine /apex-engine
COPY --from=build /out/demo-api /demo-api
EXPOSE 8700
ENTRYPOINT ["/apex-engine"]
