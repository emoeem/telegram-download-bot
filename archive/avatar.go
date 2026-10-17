package archive

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/tg"
	"github.com/iyear/tdl/archive/store"
	"github.com/iyear/tdl/pkg/consts"
)

type AvatarWorker struct {
	Store   *store.Store
	Runtime *Runtime
	Root    string
}

func NewAvatarWorker(s *store.Store, rt *Runtime, root string) *AvatarWorker {
	if root == "" {
		root = filepath.Join(consts.DataDir, "archive", "avatars")
	}
	return &AvatarWorker{Store: s, Runtime: rt, Root: root}
}
func (w *AvatarWorker) Run(ctx context.Context) error {
	jobs, err := w.Store.PendingAvatars(ctx, 100)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(w.Root, 0755); err != nil {
		return err
	}
	for _, j := range jobs {
		if err := w.download(ctx, j); err != nil {
			continue
		}
	}
	return nil
}
func (w *AvatarWorker) download(ctx context.Context, j store.AvatarJob) error {
	p, err := ResolveChatID(ctx, w.Runtime, j.ID)
	if err != nil {
		return err
	}
	loc := &tg.InputPeerPhotoFileLocation{Big: true, Peer: p.InputPeer(), PhotoID: j.PhotoID}
	tmp := filepath.Join(w.Root, fmt.Sprintf("%s-%d.tmp", j.Kind, j.ID))
	defer os.Remove(tmp)
	if _, err := downloader.NewDownloader().WithPartSize(256*1024).Download(w.Runtime.API, loc).WithThreads(2).ToPath(ctx, tmp); err != nil {
		return err
	}
	ext := filepath.Join(w.Root, fmt.Sprintf("%s-%d-%d.jpg", j.Kind, j.ID, j.PhotoID))
	if err := os.Rename(tmp, ext); err != nil {
		return err
	}
	return w.Store.SetAvatarPath(ctx, j.Kind, j.ID, ext)
}
