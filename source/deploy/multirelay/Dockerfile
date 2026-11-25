FROM golang:1.27.3 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -tags croc_no_tailcat -trimpath -o /croc .
FROM alpine:3.22
RUN addgroup -S croc && adduser -S -G croc croc
COPY --from=build /croc /usr/local/bin/croc
USER croc
ENTRYPOINT ["croc"]
