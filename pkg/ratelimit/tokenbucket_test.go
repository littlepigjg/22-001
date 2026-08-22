package ratelimit

import (
	"sync"
	"testing"
)

func TestTakeLastN_Bounds(t *testing.T) {
	cases := []struct {
		name    string
		samples []int64
		n       int
		wantLen int
	}{
		{"n larger than len (3 vs 50000)", []int64{1, 2, 3}, 50000, 3},
		{"n much larger (empty vs 100000)", []int64{}, 100000, 0},
		{"n equals len", []int64{1, 2, 3}, 3, 3},
		{"n less than len", []int64{1, 2, 3, 4, 5}, 2, 2},
		{"n is zero", []int64{1, 2, 3}, 0, 0},
		{"n negative", []int64{1, 2, 3}, -5, 0},
		{"nil samples", nil, 10, 0},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("TakeLastN panicked: %v", r)
				}
			}()
			got := TakeLastN(c.samples, c.n)
			if len(got) != c.wantLen {
				t.Fatalf("len = %d, want %d", len(got), c.wantLen)
			}
		})
	}
}

func TestTakeLastN_ReturnsLastN(t *testing.T) {
	got := TakeLastN([]int64{10, 20, 30, 40, 50}, 2)
	if len(got) != 2 || got[0] != 40 || got[1] != 50 {
		t.Fatalf("got %v, want [40 50]", got)
	}
}

func TestTakeLastN_AllWhenOverBurst(t *testing.T) {
	src := []int64{7, 8, 9}
	got := TakeLastN(src, 99999)
	if len(got) != 3 || got[0] != 7 || got[1] != 8 || got[2] != 9 {
		t.Fatalf("got %v, want all [7 8 9]", got)
	}
}

func TestDrainAndSample_BurstLargerThanAvailable(t *testing.T) {
	// burst 远大于桶容量/可成功 Take 次数，必须不 panic 且返回全部已有样本。
	bucket, err := NewTokenBucket(5, 5)
	if err != nil {
		t.Fatal(err)
	}
	rec := NewSampleRecorder(1024)
	h := NewBucketHelper(bucket, rec)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("DrainAndSample panicked: %v", r)
		}
	}()
	done, tail := h.DrainAndSample(100000)
	if done < 0 {
		t.Fatalf("done = %d, want >= 0", done)
	}
	// tail 至多等于实际成功次数（桶容量上限），绝不应为负或 panic。
	if len(tail) > int(done) {
		t.Fatalf("tail len %d exceeds done %d", len(tail), done)
	}
}

func TestSampleRecorder_AppendEvictsOldest(t *testing.T) {
	rec := NewSampleRecorder(3)
	for _, v := range []int64{1, 2, 3, 4} {
		rec.Append(v)
	}
	got := rec.Snapshot()
	// size=3, 追加 4 个后只保留最近 3 个。
	if len(got) != 3 || got[0] != 2 || got[1] != 3 || got[2] != 4 {
		t.Fatalf("got %v, want [2 3 4]", got)
	}
}

func TestTokenBucket_AllowN_ExceedsCapacity(t *testing.T) {
	b, _ := NewTokenBucket(10, 10)
	if b.AllowN(11) {
		t.Fatal("AllowN should fail when n > capacity")
	}
	if !b.AllowN(10) {
		t.Fatal("AllowN(== capacity) should succeed")
	}
	if b.AllowN(1) {
		t.Fatal("AllowN after draining should fail")
	}
}

func TestTokenBucket_ConcurrentAllowNoRace(t *testing.T) {
	b, _ := NewTokenBucket(1000, 1000)
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = b.Allow()
			_ = b.AllowN(1)
			_ = b.Tokens()
		}()
	}
	wg.Wait()
}
