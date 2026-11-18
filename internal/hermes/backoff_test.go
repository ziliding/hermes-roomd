package hermes

import (
	"testing"
	"time"
)

func TestBackoffDefaultAndCap(t *testing.T) {
	c := &Client{MaxBackoff: 5 * time.Second}
	if d := c.backoff("", 0); d != 2*time.Second {
		t.Errorf("attempt 0 = %v", d)
	}
	if d := c.backoff("", 4); d != 5*time.Second {
		t.Errorf("capped = %v", d)
	}
	if d := c.backoff("3600", 0); d != 5*time.Second {
		t.Errorf("retry-after capped = %v", d)
	}
}
