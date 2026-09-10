module github.com/alibaba/open-code-review

go 1.25.5

require (
	charm.land/bubbles/v2 v2.1.1
	charm.land/bubbletea/v2 v2.0.8
	charm.land/lipgloss/v2 v2.0.6
	github.com/alibaba/open-code-review/internal v0.0.0
	github.com/anthropics/anthropic-sdk-go v1.63.1
	github.com/aws/aws-sdk-go-v2/config v1.32.35
	github.com/bmatcuk/doublestar/v4 v4.10.0
	github.com/charmbracelet/x/term v0.2.2
	github.com/google/uuid v1.6.0
	github.com/modelcontextprotocol/go-sdk v1.7.0
	github.com/openai/openai-go/v3 v3.51.0
	github.com/pkoukk/tiktoken-go v0.1.8
	github.com/spf13/cobra v1.10.2
	github.com/spf13/pflag v1.0.10
	go.opentelemetry.io/otel v1.45.0
	go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc v1.45.0
	go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp v1.45.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.45.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.45.0
	go.opentelemetry.io/otel/exporters/stdout/stdoutmetric v1.45.0
	go.opentelemetry.io/otel/exporters/stdout/stdouttrace v1.45.0
	go.opentelemetry.io/otel/metric v1.45.0
	go.opentelemetry.io/otel/sdk v1.45.0
	go.opentelemetry.io/otel/sdk/metric v1.45.0
	go.opentelemetry.io/otel/trace v1.45.0
)

replace (
	github.com/alibaba/open-code-review/internal => ./upstream/internal
)
