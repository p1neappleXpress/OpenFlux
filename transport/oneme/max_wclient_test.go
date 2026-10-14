package oneme

import "testing"

// A call before Connect (or after a failed one) is an error, not a nil
// connection write that takes the process down.
func TestInvokeWithoutConnection(t *testing.T) {
	c := NewMaxClient()
	if _, err := c.invoke(6, map[string]interface{}{}); err == nil {
		t.Fatal("invoke without a connection succeeded")
	}
	if err := c.LoginByToken("token"); err == nil {
		t.Fatal("login without a connection succeeded")
	}
}
