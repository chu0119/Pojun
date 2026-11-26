package hostd

import "testing"

func TestUFWAllows(t *testing.T) {
	status := `Status: active

To                         Action      From
--                         ------      ----
22/tcp                     ALLOW       Anywhere
80/tcp                     ALLOW       Anywhere
443                        ALLOW       Anywhere
8000:8100/tcp              ALLOW       Anywhere
9000,9001/udp               ALLOW       Anywhere
10000/tcp                  DENY        Anywhere
9100/tcp                   ALLOW       10.0.0.0/8
22/tcp (v6)                ALLOW       Anywhere (v6)
`
	cases := map[int]bool{
		22:    true,
		80:    true,
		443:   true,
		8050:  true,
		9000:  false, // udp
		10000: false, // DENY
		8787:  false,
		9100:  false, // 仅内网来源，不等于对 LE 放行
	}
	for port, want := range cases {
		if got := ufwAllows(status, port); got != want {
			t.Errorf("port %d: got %v want %v", port, got, want)
		}
	}
}

func TestUFWAllowsInactive(t *testing.T) {
	if ufwAllows("Status: inactive\n", 80) {
		t.Fatal("inactive 不应报告已放行")
	}
}

func TestValidFirewallPort(t *testing.T) {
	for _, p := range []int{80, 443} {
		if err := validFirewallPort(p); err != nil {
			t.Errorf("%d 应允许: %v", p, err)
		}
	}
	for _, p := range []int{0, 22, 8080, 8443, 65536} {
		if err := validFirewallPort(p); err == nil {
			t.Errorf("%d 应拒绝", p)
		}
	}
}
