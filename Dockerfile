FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/lab-api ./cmd/api

FROM alpine:3.22
RUN apk add --no-cache ca-certificates poppler-utils tesseract-ocr tesseract-ocr-data-eng tesseract-ocr-data-rus && addgroup -S lab && adduser -S -G lab -u 10001 lab
WORKDIR /app
COPY --from=build /out/lab-api /usr/local/bin/lab-api
RUN mkdir -p /app/data/uploads && chown -R lab:lab /app/data
USER lab
EXPOSE 8080
ENTRYPOINT ["lab-api"]
