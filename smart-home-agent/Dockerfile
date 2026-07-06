# Multi-stage: build the static Go binary, then run it on the HA Alpine base.
# BUILD_FROM is supplied by the HA add-on builder (from build.yaml when honored);
# the default is a resilient fallback because Supervisor 2026.04.0+ no longer
# injects a default BUILD_FROM, so an un-set ARG would leave the runtime FROM
# empty and fail the build.
ARG BUILD_FROM=ghcr.io/home-assistant/base:3.19

FROM golang:1.26-alpine AS build
WORKDIR /src

# Cache module downloads separately from the source for faster rebuilds.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Static, CGO-free binary so it runs on the minimal Alpine base unchanged.
ARG AGENT_VERSION=dev
RUN CGO_ENABLED=0 go build \
    -ldflags "-s -w -X github.com/smart-home/edge/agent/internal/config.AgentVersion=${AGENT_VERSION}" \
    -o /out/smart-home-agent .

FROM ${BUILD_FROM}
# s6-overlay (present in HA base images) will start the command below.
COPY --from=build /out/smart-home-agent /usr/bin/smart-home-agent
COPY run.sh /
RUN chmod a+x /run.sh

# Must go through with-contenv (see run.sh) so SUPERVISOR_TOKEN is available.
CMD ["/run.sh"]
