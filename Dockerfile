FROM golang:1.23 AS build
WORKDIR /workspace
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /workspace/bin/per-user-container-operator ./cmd

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=build /workspace/bin/per-user-container-operator /per-user-container-operator
USER 65532:65532
ENTRYPOINT ["/per-user-container-operator"]
