FROM golang:1.27.1-alpine AS build
WORKDIR /src
COPY examples/hello/main.go .
RUN CGO_ENABLED=0 go build -trimpath -o /hello main.go

FROM scratch
COPY --from=build /hello /hello
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/hello"]
