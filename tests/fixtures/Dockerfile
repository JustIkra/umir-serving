# syntax=docker/dockerfile:1
FROM golang:1.26.9-alpine@sha256:3082400e369fa24d5fc60bca20edab3f6d604e0c5a690ec66b295eff4dd87ade AS build
WORKDIR /app
ENV GOWORK=off
COPY go.mod ./
RUN go mod download
COPY bootstrap ./bootstrap
COPY contract ./contract
COPY internal ./internal
COPY tests/fixtures ./tests/fixtures
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /out/fixture-server ./tests/fixtures/server && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /out/fixture-control ./tests/fixtures/control
RUN go run ./tests/fixtures/catalog/build
RUN mkdir -p /out/run/umir-fixtures/config /out/run/umir-fixtures/secrets /out/run/umir-fixtures/private && chmod 0700 /out/run/umir-fixtures /out/run/umir-fixtures/config /out/run/umir-fixtures/secrets /out/run/umir-fixtures/private && chown -R 65532:65532 /out/run

FROM scratch AS runtime
WORKDIR /app
COPY --from=build /out/fixture-server /fixture-server
COPY --from=build /out/fixture-control /fixture-control
COPY --from=build /out/zoneinfo.zip /usr/local/lib/umir/zoneinfo.zip
COPY --from=build /out/catalog-provenance.json /usr/local/lib/umir/catalog-provenance.json
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/run /run
LABEL org.opencontainers.image.source="https://github.com/JustIkra/umir-serving" org.umir.fixture.protocol="umir-operational-fixtures/1.1.0"
USER 65532:65532
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 CMD ["/fixture-server", "healthcheck"]
ENTRYPOINT ["/fixture-server"]
CMD ["serve"]
