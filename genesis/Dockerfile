FROM golang:1.17.8-alpine3.15 AS build
WORKDIR /build
RUN apk upgrade --no-cache \
    && apk add --no-cache \
    nodejs npm yarn git make python2 python3 g++ musl-dev linux-headers bash
COPY package.json yarn.lock ./
RUN yarn
COPY . ./
RUN yarn compile
RUN go build -o /build/create-genesis ./

FROM golang:1.18-alpine3.16 AS run
WORKDIR /build
COPY --from=build /build/create-genesis /build/create-genesis
RUN chmod +x /build/create-genesis
ENTRYPOINT ["/build/create-genesis"]
