package uia

import "testing"

func TestLooksLikeAddressBar(t *testing.T) {
	if !LooksLikeAddressBar(FocusInfo{Name: "Address and search bar", ControlType: ControlTypeEdit}) {
		t.Fatal("english omnibox name should match")
	}
	if !LooksLikeAddressBar(FocusInfo{Name: "地址和搜索栏", ControlType: ControlTypeEdit}) {
		t.Fatal("chinese omnibox name should match")
	}
	if LooksLikeAddressBar(FocusInfo{Name: "Address and search bar", ControlType: ControlTypeDocument}) {
		t.Fatal("a document is not the address bar")
	}
	if LooksLikeAddressBar(FocusInfo{Name: "Some random edit", ControlType: ControlTypeEdit}) {
		t.Fatal("arbitrary edits must not match")
	}
}
