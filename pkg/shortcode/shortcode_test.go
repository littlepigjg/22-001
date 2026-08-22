package shortcode

import (
	"strings"
	"testing"
)

// newTestGenerator returns a generator with default settings for tests.
func newTestGenerator(t *testing.T) *Generator {
	t.Helper()
	g, err := New("", 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return g
}

// TestGenerateManyDistinct 验证批量生成的每条短码互不相同。
// 这是回归测试：原先 GenerateMany 复用共享 scratch 缓冲并使用
// unsafe.Pointer 转成 string，导致切片里所有 string 指向同一块内存，
// 最终全都等于最后一次生成的内容。
func TestGenerateManyDistinct(t *testing.T) {
	g := newTestGenerator(t)

	const n = 10
	codes, err := g.GenerateMany(n)
	if err != nil {
		t.Fatalf("GenerateMany: %v", err)
	}
	if len(codes) != n {
		t.Fatalf("got %d codes, want %d", len(codes), n)
	}

	seen := make(map[string]struct{}, n)
	for i, c := range codes {
		if c == "" {
			t.Fatalf("codes[%d] is empty", i)
		}
		if _, dup := seen[c]; dup {
			t.Fatalf("duplicate code within batch: codes[%d]=%q (all codes: %v)", i, c, codes)
		}
		seen[c] = struct{}{}
	}
}

// TestGenerateManyNoAliasing 验证返回切片中的每个 string 拥有独立的
// 底层存储，不会因为后续修改而被改写。这里在拿到结果后立即再生成一批，
// 然后断言第一批的内容保持不变。
func TestGenerateManyNoAliasing(t *testing.T) {
	g := newTestGenerator(t)

	first, err := g.GenerateMany(10)
	if err != nil {
		t.Fatalf("GenerateMany first: %v", err)
	}
	// 复制快照，作为不可变参照。
	snapshot := make([]string, len(first))
	copy(snapshot, first)

	// 再次生成，触发内部缓冲/状态的复用路径。
	if _, err := g.GenerateMany(10); err != nil {
		t.Fatalf("GenerateMany second: %v", err)
	}
	// 再单独生成几次，覆盖 Generate 路径。
	for i := 0; i < 5; i++ {
		if _, err := g.Generate(); err != nil {
			t.Fatalf("Generate: %v", err)
		}
	}

	for i, got := range first {
		if got != snapshot[i] {
			t.Fatalf("first[%d] mutated after later calls: got %q, want %q", i, got, snapshot[i])
		}
	}
}

// TestGenerateManySubsequentCallDistinct 验证两次连续调用 GenerateMany
// 之间不产生错误的交叉复用（第一批整体不应等于第二批整体）。
func TestGenerateManySubsequentCallDistinct(t *testing.T) {
	g := newTestGenerator(t)

	a, err := g.GenerateMany(10)
	if err != nil {
		t.Fatalf("GenerateMany a: %v", err)
	}
	b, err := g.GenerateMany(10)
	if err != nil {
		t.Fatalf("GenerateMany b: %v", err)
	}

	ja := strings.Join(a, "")
	jb := strings.Join(b, "")
	if ja == jb {
		t.Fatalf("two consecutive GenerateMany calls produced identical joined output: %q", ja)
	}
}

// TestGenerateManyErrors 验证非法入参返回错误。
func TestGenerateManyErrors(t *testing.T) {
	g := newTestGenerator(t)
	if _, err := g.GenerateMany(0); err == nil {
		t.Fatal("GenerateMany(0) = nil, want error")
	}
	if _, err := g.GenerateMany(-1); err == nil {
		t.Fatal("GenerateMany(-1) = nil, want error")
	}
}
