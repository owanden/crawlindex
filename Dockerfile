FROM golang:1.26.8-bookworm AS source
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

FROM source AS test
CMD ["go", "test", "-race", "-count=1", "./..."]

FROM source AS build
RUN CGO_ENABLED=0 go build -trimpath -o /crawlindex ./cmd/crawlindex

FROM gcr.io/distroless/static-debian12:nonroot AS runtime
COPY --from=build /crawlindex /crawlindex
COPY --from=build /go/pkg/mod/github.com/jimsmart/grobotstxt@v1.0.3/LICENSE /licenses/grobotstxt-LICENSE
ENTRYPOINT ["/crawlindex"]
