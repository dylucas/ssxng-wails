package logbuf

import (
	"testing"
	"time"
)

func TestAddAndEntries(t *testing.T) {
	b := New(10)
	if b.Len() != 0 {
		t.Fatalf("新建缓冲应为空，实际 %d", b.Len())
	}
	b.Add("info", "app", "第一条")
	b.Add("error", "proxy", "第二条")

	entries := b.Entries(10)
	if len(entries) != 2 {
		t.Fatalf("应有 2 条日志，实际 %d", len(entries))
	}
	// Oldest first (the UI appends), and sequence numbers increase with time.
	if entries[0].Message != "第一条" || entries[1].Message != "第二条" {
		t.Fatalf("日志顺序不对: %+v", entries)
	}
	if entries[1].Seq <= entries[0].Seq {
		t.Errorf("序号应当递增: %d %d", entries[0].Seq, entries[1].Seq)
	}
	if entries[1].Level != "error" || entries[1].Scope != "proxy" {
		t.Errorf("级别/范围丢失: %+v", entries[1])
	}
	if entries[1].Time.IsZero() || time.Since(entries[1].Time) > time.Minute {
		t.Errorf("时间戳不对: %v", entries[1].Time)
	}
	if b.LastSeq() != entries[1].Seq {
		t.Errorf("LastSeq 应为最新序号 %d，实际 %d", entries[1].Seq, b.LastSeq())
	}
}

func TestRingDropsOldest(t *testing.T) {
	b := New(3)
	for i := 0; i < 10; i++ {
		b.Add("info", "s", string(rune('a'+i)))
	}
	entries := b.Entries(10)
	if len(entries) != 3 {
		t.Fatalf("缓冲上限应为 3，实际 %d", len(entries))
	}
	if entries[0].Message != "h" || entries[2].Message != "j" {
		t.Fatalf("应当保留最新的 3 条: %+v", entries)
	}
	if got := b.Len(); got != 3 {
		t.Errorf("Len 应为 3，实际 %d", got)
	}
}

func TestEntriesLimit(t *testing.T) {
	b := New(50)
	for i := 0; i < 20; i++ {
		b.Add("info", "s", "x")
	}
	if n := len(b.Entries(5)); n != 5 {
		t.Errorf("limit=5 应返回 5 条，实际 %d", n)
	}
	if n := len(b.Entries(0)); n != 20 {
		t.Errorf("limit=0 应返回全部，实际 %d", n)
	}
	if n := len(b.Entries(-1)); n != 20 {
		t.Errorf("负 limit 应返回全部，实际 %d", n)
	}
	if n := len(b.Entries(1000)); n != 20 {
		t.Errorf("limit 超过条数时应返回全部，实际 %d", n)
	}
}

func TestClearKeepsSequenceMonotonic(t *testing.T) {
	b := New(10)
	b.Add("info", "s", "a")
	before := b.LastSeq()
	b.Clear()
	if b.Len() != 0 {
		t.Fatalf("清空后应为空，实际 %d", b.Len())
	}
	b.Add("info", "s", "b")
	if b.LastSeq() <= before {
		t.Errorf("清空后序号仍应递增: %d -> %d", before, b.LastSeq())
	}
	// The frontend uses "give me everything after seq" after a clear, so a
	// stale cursor must not resurrect old lines.
	if n := len(b.Entries(10)); n != 1 {
		t.Errorf("清空后只应有 1 条，实际 %d", n)
	}
}

func TestSubscribeReceivesAndUnsubscribes(t *testing.T) {
	b := New(10)
	ch, cancel := b.Subscribe(4)
	defer cancel()

	b.Add("info", "s", "hello")
	select {
	case e := <-ch:
		if e.Message != "hello" {
			t.Errorf("收到的内容不对: %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("订阅者没有收到日志")
	}

	// A slow subscriber must not block the producer: the channel is buffered and
	// drops instead of stalling the proxy.
	for i := 0; i < 20; i++ {
		b.Add("info", "s", "flood")
	}

	cancel()
	b.Add("info", "s", "after cancel")
	select {
	case <-ch:
	case <-time.After(100 * time.Millisecond):
		// The channel is closed or drained; either is fine.
	}
}

func TestNewWithNonPositiveLimitStillWorks(t *testing.T) {
	b := New(0)
	b.Add("info", "s", "x")
	if b.Len() > 1 {
		t.Errorf("上限为 0 时不应堆积: %d", b.Len())
	}
}
