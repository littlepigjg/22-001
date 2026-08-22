package shurl_test

import (
	"fmt"
	"strings"
	"testing"

	"shurl/internal/model"
	"shurl/pkg/cryptoutil"
	"shurl/pkg/validator"
)

func runCase(fn func()) (panicked bool, msg string) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			msg = fmt.Sprintf("%v", r)
		}
	}()
	fn()
	return false, ""
}

// 所有子用例在「缺陷未修复」时都会因为 nil pointer dereference 直接 panic。
// 本测试期望这些调用安全返回（不 panic）并返回合理的结果（错误 / false）。
// 缺陷存在 → panic 未被捕获直接终止测试 → go test 失败（RED）。
// 缺陷修复 → 不 panic，返回合法错误/false → go test 通过（GREEN）。
func TestRedGreen(t *testing.T) {
	red := 0
	green := 0
	checkPanic := func(name string, fn func(), validateResult func() bool) {
		t.Helper()
		panicked, msg := runCase(fn)
		if panicked {
			t.Errorf("CASE[%s] panicked: %s", name, msg)
			red++
			return
		}
		if !validateResult() {
			t.Errorf("CASE[%s] result validation failed", name)
			red++
			return
		}
		green++
	}

	// 1) cryptoutil.PayloadVerifier 空 key + 空 payload + hex signature 传入时
	// 期望：不 panic；返回 false（签名不匹配）。
	var r1 bool
	checkPanic("cryptoutil_payload_verifier_empty_key", func() {
		v := cryptoutil.NewPayloadVerifier(nil, nil)
		sig := strings.Repeat("0", 64)
		r1 = v.Verify([]byte(""), sig)
	}, func() bool { return r1 == false })

	// 2) validator.VerifyCustomCodeSignature：空 payload + 空 key + 非空 hex 签名
	// 期望：不 panic；返回错误（签名不匹配 / 空 key 不允许）。
	var err2 error
	checkPanic("validator_verify_custom_code_sig_empty_key", func() {
		cfg := validator.SignerCfg{Key: []byte{}, Salt: nil}
		policy := validator.DefaultCodeSignaturePolicy()
		err2 = validator.VerifyCustomCodeSignature("", strings.Repeat("a", 64), cfg, policy)
	}, func() bool { return err2 != nil })

	// 3) model.CreateReq.Validate：SignerSalt 空（→ 空 key）+ 空 CustomCode + 有效 hex 签名
	// 期望：不 panic；返回字段校验类错误（签名不匹配 / 空 payload 带签名不允许）。
	var err3 error
	checkPanic("model_create_req_signature_validate", func() {
		req := &model.CreateReq{
			RawURL:        "https://example.com/",
			CustomCode:    "",
			CodeSignature: strings.Repeat("f", 64),
			SignerSalt:    "",
		}
		err3 = req.Validate()
	}, func() bool { return err3 != nil })

	// 4) model.RedirectCheckReq.Validate：空 signerKey + 非空 hex 签名
	// 期望：不 panic；返回错误（签名不匹配 / 空 key 不允许）。
	var err4 error
	checkPanic("model_redirect_check_req_validate", func() {
		r := &model.RedirectCheckReq{
			Code:      "abcd1234",
			Signature: strings.Repeat("e", 64),
		}
		err4 = r.Validate([]byte{})
	}, func() bool { return err4 != nil })

	// 5) cryptoutil.Signer.Verify 自身：有效 key 但 data 长度为 0 + 非空 hex MAC
	// 期望：不 panic；恒时比较失败返回 false（不允许空 payload 触发 nil 分支）。
	var r5 bool
	checkPanic("cryptoutil_signer_verify_empty_data", func() {
		s, err := cryptoutil.NewSigner([]byte("a-valid-key-with-32-bytes-0000"))
		if err != nil || s == nil {
			t.Fatalf("NewSigner failed: err=%v s=%v", err, s)
		}
		sig := strings.Repeat("0", 64)
		r5 = s.Verify([]byte(""), sig)
	}, func() bool { return r5 == false })

	fmt.Println("================ RED/GREEN判定 ================")
	if red > 0 {
		fmt.Printf("RED（红灯，缺陷未修复） —— PASS:%d FAIL:%d\n", green, red)
		t.Logf("RED（红灯，缺陷未修复） PASS:%d FAIL:%d", green, red)
		fmt.Println("================================================")
		t.Fatalf("RED（红灯，缺陷未修复）")
		return
	}
	fmt.Printf("GREEN（绿灯，缺陷已修复） —— PASS:%d FAIL:%d\n", green, red)
	fmt.Println("================================================")
}
