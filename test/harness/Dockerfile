# syntax=docker/dockerfile:1
# Test-only image for the behaviour suite's stubs (decision-waf, tinyauth, echo).
# Built locally by the harness from the test/ directory; never published.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY harness/stub ./harness/stub
RUN CGO_ENABLED=0 go build -trimpath -o /stub ./harness/stub

FROM scratch
COPY --from=build /stub /stub
USER 65534
ENTRYPOINT ["/stub"]
