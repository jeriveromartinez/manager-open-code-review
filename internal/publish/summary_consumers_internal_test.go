package publish

import (
	"strings"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func TestRenderConsumerBlockShowsPinnedSHA(t *testing.T) {
	block := renderConsumerBlock(store.ArchitectureConsumers{
		Status:     store.ConsumersComplete,
		Configured: 1,
		Checked:    1,
		Consumers: []store.ArchitectureConsumer{{
			Repository: "acme/consumer",
			Contract:   "api/openapi.yaml",
			Ref:        "v1.2.0",
			SHA:        "0123456789abcdef0123456789abcdef01234567",
			Status:     store.ConsumerStatusAffected,
			Impact:     store.ConsumerImpactVerified,
		}},
	}, func(value string) string { return value })

	for _, want := range []string{"Consumidores del contrato", "acme/consumer", "api/openapi.yaml", "v1.2.0", "0123456", "afectado", "comprobado"} {
		if !strings.Contains(block, want) {
			t.Errorf("block does not contain %q:\n%s", want, block)
		}
	}
}

func TestRenderConsumerBlockRedactsPrivateConsumer(t *testing.T) {
	block := renderConsumerBlock(store.ArchitectureConsumers{
		Status:        store.ConsumersUnknown,
		Configured:    1,
		Checked:       1,
		RequiresHuman: true,
		Consumers: []store.ArchitectureConsumer{{
			Repository: "acme/private-consumer",
			Contract:   "internal/contracts/openapi.yaml",
			Ref:        "v1.2.0",
			SHA:        "0123456789abcdef0123456789abcdef01234567",
			Status:     store.ConsumerStatusUnknown,
			Impact:     store.ConsumerImpactUnknown,
			Private:    true,
			Redacted:   true,
		}},
	}, func(value string) string { return value })

	for _, leaked := range []string{"acme/private-consumer", "internal/contracts/openapi.yaml", "v1.2.0", "0123456"} {
		if strings.Contains(block, leaked) {
			t.Errorf("block leaked %q:\n%s", leaked, block)
		}
	}
	for _, want := range []string{"consumidor privado", "requiere criterio humano", "desconocido"} {
		if !strings.Contains(block, want) {
			t.Errorf("block does not contain %q:\n%s", want, block)
		}
	}
}

func TestRenderConsumerBlockEmptyWhenNotConfigured(t *testing.T) {
	if block := renderConsumerBlock(store.ArchitectureConsumers{Status: store.ConsumersNotConfigured},
		func(value string) string { return value }); block != "" {
		t.Errorf("block = %q, want empty", block)
	}
}
