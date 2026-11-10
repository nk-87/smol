# A static binary on scratch: nothing to run as root with, nothing to write to,
# so the Deployment's runAsNonRoot / readOnlyRootFilesystem settings hold.
FROM golang:1.24-alpine AS build

ARG GIT_SHA=unknown
WORKDIR /src

COPY go.mod ./
RUN go mod download

COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags "-s -w -X main.gitSHA=${GIT_SHA}" \
      -o /out/smol .

FROM scratch
COPY --from=build /out/smol /smol
USER 65534:65534
EXPOSE 8080
ENTRYPOINT ["/smol"]
