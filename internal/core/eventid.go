package core

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strings"
)

// This namespace is part of RedDotRelay's v1 identity contract and must not
// change. It is not a secret or a deployment setting.
var eventIDNamespace = [16]byte{
	0x68, 0x36, 0x48, 0x57, 0xb5, 0xd3, 0x4b, 0x6d,
	0xb8, 0x3a, 0x5b, 0xdf, 0xcb, 0x68, 0x18, 0x6d,
}

// EventGUID returns an RFC 4122 UUID v5 derived from the durable event
// identity: chain ID, normalized transaction hash, and log index.
func EventGUID(id EventID) string {
	name := fmt.Sprintf("%d:%s:%d", id.ChainID, strings.ToLower(id.TransactionHash), id.LogIndex)
	return namespacedGUID(eventIDNamespace, name)
}

// DeliveryGUID identifies one durable event destination without exposing the
// destination URL or secret reference.
func DeliveryGUID(id EventID, destination string) string {
	return namespacedGUID(eventIDNamespace, EventGUID(id)+":"+destination)
}

func namespacedGUID(namespace [16]byte, name string) string {
	input := make([]byte, 0, len(eventIDNamespace)+len(name))
	input = append(input, namespace[:]...)
	input = append(input, name...)
	digest := sha1.Sum(input)
	identifier := digest[:16]
	identifier[6] = (identifier[6] & 0x0f) | 0x50
	identifier[8] = (identifier[8] & 0x3f) | 0x80
	hexadecimal := hex.EncodeToString(identifier)
	return fmt.Sprintf("%s-%s-%s-%s-%s", hexadecimal[:8], hexadecimal[8:12],
		hexadecimal[12:16], hexadecimal[16:20], hexadecimal[20:])
}
