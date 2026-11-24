package ticketing

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// 这些用例连真实 Echo 库，库不可用时自动跳过，保证无数据库环境下 go test 依然通过。
func newTestClient(t *testing.T) *Client {
	t.Helper()
	if os.Getenv("TICKETING_INTEGRATION_TEST") != "1" {
		t.Skip("set TICKETING_INTEGRATION_TEST=1 to test a running Echo")
	}
	client := NewFromEnv()
	if !client.Available() {
		t.Skipf("ticketing db unavailable: %v", client.Err())
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestSearchShows(t *testing.T) {
	client := newTestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	shows, err := client.SearchShows(ctx, "", 3)
	if err != nil {
		t.Fatalf("search shows: %v", err)
	}
	if len(shows) == 0 {
		t.Fatal("expected seeded shows, got none")
	}
	if shows[0].Name == "" {
		t.Fatal("show name should not be empty")
	}
}

func TestListTicketsByShow(t *testing.T) {
	client := newTestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	shows, err := client.SearchShows(ctx, "", 1)
	if err != nil || len(shows) == 0 {
		t.Fatalf("need a show to test tickets: %v", err)
	}
	tiers, err := client.ListTickets(ctx, shows[0].ID)
	if err != nil {
		t.Fatalf("list tickets: %v", err)
	}
	for _, tier := range tiers {
		// 库里是分，对外必须是元；这里守住换算约定。
		if tier.Price <= 0 {
			t.Fatalf("ticket price should be positive, got %v", tier.Price)
		}
		if tier.Price > 100000 {
			t.Fatalf("ticket price looks like cents not yuan: %v", tier.Price)
		}
	}
}

func TestListSeckillTickets(t *testing.T) {
	client := newTestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tiers, err := client.ListSeckillTickets(ctx, 3)
	if err != nil {
		t.Fatalf("list seckill tickets: %v", err)
	}
	for _, tier := range tiers {
		if tier.ShowName == "" {
			t.Fatal("seckill ticket should carry show name")
		}
	}
}

func TestAreaUsesStandardPlaceNameWithoutBreakingFilters(t *testing.T) {
	for input, want := range map[string]string{"香港": "中国香港", "澳门特别行政区": "中国澳门", "台湾": "中国台湾", "上海": "上海", "朝阳区": "朝阳区"} {
		if got := (apiShow{Area: input}).toShow().Area; got != want {
			t.Fatalf("%q became %q, want %q", input, got, want)
		}
	}
	if !strings.Contains((apiShow{Area: "香港"}).toShow().Area, "香港") {
		t.Fatal("city filtering still needs the original name inside the display value")
	}
}
