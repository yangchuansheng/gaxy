FROM golang:1.21 AS build
WORKDIR /go/src/github.com/duyet/gaxy
COPY . .
RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -installsuffix cgo -o gaxy .

# Add Tini
ENV TINI_VERSION v0.19.0
ADD https://github.com/krallin/tini/releases/download/${TINI_VERSION}/tini /tini
RUN chmod +x /tini

FROM alpine:latest
WORKDIR /app
ENV ROUTE_PREFIX \
    GOOGLE_ORIGIN \
    INJECT_PARAMS_FROM_REQ_HEADERS \
    SKIP_PARAMS_FROM_REQ_HEADERS \
    PORT
COPY --from=build /go/src/github.com/duyet/gaxy/gaxy .
COPY --from=build /tini /tini
ENTRYPOINT ["/tini", "--"]
EXPOSE 3000
CMD ["./gaxy"]
