# Build stage
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
# COPY go.sum ./        # uncomment once external deps land in step 2
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /merger ./cmd/merger

# Runtime stage — exiftool is already here for the merge step
FROM alpine:3.21
RUN apk add --no-cache exiftool tzdata
COPY --from=build /merger /usr/local/bin/merger
ENTRYPOINT ["merger"]
CMD ["--help"]
