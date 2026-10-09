package nono

import (
	"os"
	"testing"
)

func TestStoreDeleteCredentialLive(test *testing.T) {
	if os.Getenv("AIP_HARDWARE_TESTS") == "" {
		test.Skip("set AIP_HARDWARE_TESTS=1 to run the live keychain test")
	}
	name := "ai-platform-test-cred"
	if err := StoreCredential(name, "sk-test-12345"); err != nil {
		test.Fatalf("StoreCredential: %v", err)
	}
	if err := DeleteCredential(name); err != nil {
		test.Fatalf("DeleteCredential: %v", err)
	}
}
