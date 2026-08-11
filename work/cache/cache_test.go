package cache

import (
	"testing"
	"time"
)

func TestXCDataDistinguishesValidEmptyFromMissing(t *testing.T) {
	c, err := NewCache(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, found := c.GetXCData("valid-empty"); found {
		t.Fatal("missing entry reported as present")
	}
	c.SetXCData("valid-empty", "")
	if value, found := c.GetXCData("valid-empty"); !found || value != "" {
		t.Fatalf("valid-empty entry = %q, found=%t", value, found)
	}
}
