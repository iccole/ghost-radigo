# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /app
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /station ./cmd/station

# whisper.cpp stage: whisper-cli transcribes speech windows so pieces can be
# joined grammatically. Static build, portable CPU flags.
FROM alpine:3.21 AS whisper

RUN apk add --no-cache build-base cmake git
RUN git clone --depth 1 --branch v1.9.5 https://github.com/ggml-org/whisper.cpp /src \
 && cmake -S /src -B /build -DCMAKE_BUILD_TYPE=Release -DBUILD_SHARED_LIBS=OFF \
      -DGGML_NATIVE=OFF -DWHISPER_BUILD_TESTS=OFF -DWHISPER_BUILD_SERVER=OFF \
 && cmake --build /build --target whisper-cli -j
ADD --chmod=644 https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-base.en.bin /models/ggml-base.en.bin

# Runtime stage
FROM alpine:3.21

# ffmpeg is called as a subprocess by the processor workers; whisper-cli
# needs the C++ and OpenMP runtimes.
RUN apk add --no-cache ffmpeg ca-certificates libstdc++ libgomp

COPY --from=builder /station /usr/local/bin/station
COPY --from=whisper /build/bin/whisper-cli /usr/local/bin/whisper-cli
COPY --from=whisper /models /models

ENV GHOST_RADIO_WHISPER_MODEL=/models/ggml-base.en.bin

EXPOSE 8080

ENTRYPOINT ["station"]
