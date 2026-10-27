// Package telemetry defines the tracing and metrics interfaces of agentx and
// carries spans and run state through contexts. Adapt OpenTelemetry or
// another library to Tracer and Meter; agentx itself has no telemetry
// dependency. The middleware package records the spans and metrics of agent
// runs with these interfaces, following the OpenTelemetry GenAI semantic
// conventions, and other packages, such as hook and actx, add their spans
// with StartSpan.
package telemetry
