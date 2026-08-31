package core

import (
	"strings"
	"testing"
)

func TestEventGUIDIsStableAndChainAware(t *testing.T) {
	id := EventID{
		ChainID:         9171317,
		TransactionHash: "0xf5cbc703549d9af65e5bb50a797858aaf79a740f7d6c5cd707220c01e4d8a9e3",
		LogIndex:        0,
	}
	const expected = "bd493388-c02e-5299-aa84-6d8504c48905"
	if got := EventGUID(id); got != expected {
		t.Fatalf("EventGUID() = %q, want %q", got, expected)
	}
	id.TransactionHash = strings.ToUpper(id.TransactionHash)
	if got := EventGUID(id); got != expected {
		t.Fatalf("EventGUID() with uppercase hash = %q, want %q", got, expected)
	}
	id.ChainID++
	if got := EventGUID(id); got == expected {
		t.Fatal("EventGUID() did not distinguish chain IDs")
	}
}

func TestDeliveryGUIDIsStableAndDestinationAware(t *testing.T) {
	id := EventID{ChainID: 1, TransactionHash: "0xabc", LogIndex: 7}
	first := DeliveryGUID(id, "env://WEBHOOK_URL")
	if first == "" || first != DeliveryGUID(id, "env://WEBHOOK_URL") || first == DeliveryGUID(id, "env://OTHER") {
		t.Fatalf("delivery GUID identity is not stable and destination-aware: %q", first)
	}
}
