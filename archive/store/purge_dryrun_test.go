package store

import (
	"context"
	"fmt"
	"testing"
)

// dry-run 预览必须与 purgeItemsTx 的 refcount 语义一致：内容去重共享的对象，
// 只剩最后一个引用时才列入待删文件——否则操作员按预览核对「清空」时会拿到
// 一个夸大的 bytes/文件清单。
func TestPurgeDryRunMatchesRefcountReality(t *testing.T) {
	s := openSerialStore(t)
	ctx := context.Background()
	first := seedMedia(t, s, -100, 1, 1700000000, "photo", false)
	second := seedMedia(t, s, -100, 2, 1700000001, "photo", false)
	sha := "dd" + fmt.Sprintf("%062d", 4)
	obj := "/tmp/obj/" + sha
	for _, id := range []int64{first, second} {
		if _, err := s.DB().Exec("UPDATE media SET status='completed',sha256=?,original_path=? WHERE id=?", sha, obj, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DB().Exec("INSERT INTO files(sha256,size,mime_type,state,ref_count,created_at) VALUES(?,100,'image/jpeg','ready',2,1)", sha); err != nil {
		t.Fatal(err)
	}
	serialOf := func(id int64) int64 {
		t.Helper()
		var serial int64
		if err := s.DB().QueryRow("SELECT serial FROM media WHERE id=?", id).Scan(&serial); err != nil {
			t.Fatal(err)
		}
		return serial
	}
	firstSerial, secondSerial := serialOf(first), serialOf(second)

	// 只删第一个引用：dry-run 不得把共享对象列入待删清单。
	rep, err := s.Purge(ctx, PurgeSelector{Serial: firstSerial, Messages: true, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Files) != 0 {
		t.Fatalf("dry-run 把仍被引用的共享对象列入了删除: %v", rep.Files)
	}
	var n int
	if err := s.DB().QueryRow("SELECT COUNT(*) FROM media").Scan(&n); err != nil || n != 2 {
		t.Fatalf("dry-run 删除了行: media=%d err=%v", n, err)
	}

	// 真实删除第一个引用：不报文件（共享对象仍在被引用），ref_count 减到 1。
	rep, err = s.Purge(ctx, PurgeSelector{Serial: firstSerial, Messages: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Files) != 0 {
		t.Fatalf("删除共享引用之一不应报告文件: %v", rep.Files)
	}
	var left int
	if err := s.DB().QueryRow("SELECT ref_count FROM files WHERE sha256=?", sha).Scan(&left); err != nil || left != 1 {
		t.Fatalf("ref_count=%d want 1 err=%v", left, err)
	}

	// 此时第二个引用就是最后一个：dry-run 恰好列入一次。
	rep, err = s.Purge(ctx, PurgeSelector{Serial: secondSerial, Messages: true, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Files) != 1 || rep.Files[0] != obj {
		t.Fatalf("dry-run 应只列最终对象一次: %v", rep.Files)
	}

	// 真实删除最后一个引用：报告一次、files 行随 ref_count 归零消失。
	rep, err = s.Purge(ctx, PurgeSelector{Serial: secondSerial, Messages: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Files) != 1 || rep.Files[0] != obj {
		t.Fatalf("执行删除应报告文件一次: %v", rep.Files)
	}
	if err := s.DB().QueryRow("SELECT COUNT(*) FROM files WHERE sha256=?", sha).Scan(&n); err != nil || n != 0 {
		t.Fatalf("files 行应随最后一个引用消失: n=%d err=%v", n, err)
	}
}
