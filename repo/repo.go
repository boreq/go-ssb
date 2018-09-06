package repo

import (
	"context"
	"fmt"
	"os"
	"path"

	"github.com/cryptix/go/logging"
	"github.com/dgraph-io/badger"
	"github.com/pkg/errors"

	"go.cryptoscope.co/librarian"
	libbadger "go.cryptoscope.co/librarian/badger"
	"go.cryptoscope.co/luigi"
	"go.cryptoscope.co/margaret"
	"go.cryptoscope.co/margaret/codec/msgpack"
	"go.cryptoscope.co/margaret/multilog"
	multibadger "go.cryptoscope.co/margaret/multilog/badger"
	"go.cryptoscope.co/secretstream/secrethandshake"

	"go.cryptoscope.co/sbot"
	"go.cryptoscope.co/sbot/blobstore"
)

var _ Interface = (*repo)(nil)

var check = logging.CheckFatal

// New creates a new repository value, it opens the keypair and database from basePath if it is already existing
func New(log logging.Interface, basePath string, opts ...Option) (Interface, error) {
	r := &repo{basePath: basePath, log: log}

	for i, o := range opts {
		err := o(r)
		if err != nil {
			return nil, errors.Wrapf(err, "repo: failed to apply option %d", i)
		}
	}

	if r.ctx == nil {
		r.ctx = context.Background()
	}
	r.ctx, r.shutdown = context.WithCancel(r.ctx)

	var err error
	r.blobStore, err = r.getBlobStore()
	if err != nil {
		return nil, errors.Wrap(err, "error creating blob store")
	}

	if r.keyPair == nil {
		r.keyPair, err = r.getKeyPair()
		if err != nil {
			return nil, errors.Wrap(err, "error reading KeyPair")
		}
	}

	return r, nil
}

type repo struct {
	ctx      context.Context
	log      logging.Interface
	shutdown func()
	basePath string

	blobStore sbot.BlobStore
	keyPair   *sbot.KeyPair
}

func (r repo) Close() error {
	r.shutdown()
	// FIXME: does shutdown block..?
	// would be good to get back some kind of _all done without a problem_
	// time.Sleep(1 * time.Second)

	var err error

	return err
}

func (r *repo) GetPath(rel ...string) string {
	return path.Join(append([]string{r.basePath}, rel...)...)
}

func (r *repo) getKeyPair() (*sbot.KeyPair, error) {
	if r.keyPair != nil {
		return r.keyPair, nil
	}

	var err error
	secPath := r.GetPath("secret")
	r.keyPair, err = sbot.LoadKeyPair(secPath)
	if err != nil {
		if !os.IsNotExist(errors.Cause(err)) {
			return nil, errors.Wrap(err, "error opening key pair")
		}
		// generating new keypair
		kp, err := secrethandshake.GenEdKeyPair(nil)
		if err != nil {
			return nil, errors.Wrap(err, "error building key pair")
		}
		r.keyPair = &sbot.KeyPair{
			Id:   &sbot.FeedRef{ID: kp.Public[:], Algo: "ed25519"},
			Pair: *kp,
		}
		// TODO:
		// keyFile, err := os.Create(secPath)
		// if err != nil {
		// 	return nil, errors.Wrap(err, "error creating secret file")
		// }
		// if err:=sbot.SaveKeyPair(keyFile);err != nil {
		// 	return nil, errors.Wrap(err, "error saving secret file")
		// }
		fmt.Println("warning: save new keypair!")
	}

	return r.keyPair, nil
}

func (r *repo) KeyPair() sbot.KeyPair {
	return *r.keyPair
}

func (r *repo) getBlobStore() (sbot.BlobStore, error) {
	if r.blobStore != nil {
		return r.blobStore, nil
	}

	bs, err := blobstore.New(path.Join(r.basePath, "blobs"))
	if err != nil {
		return nil, errors.Wrap(err, "error creating blob store")
	}

	r.blobStore = bs
	return bs, nil
}

func (r *repo) BlobStore() sbot.BlobStore {
	return r.blobStore
}

// GetMultiLog uses the repo to determine the paths where to finds the multilog with given name and opens it.
//
// Exposes the badger db for 100% hackability. This will go away in future versions!
func GetMultiLog(r Interface, name string, f multilog.Func) (multilog.MultiLog, *badger.DB, func(context.Context, margaret.Log) error, error) {
	// badger + librarian as index
	opts := badger.DefaultOptions

	dbPath := r.GetPath("sublogs", name, "db")
	err := os.MkdirAll(dbPath, 0700)
	if err != nil {
		return nil, nil, nil, errors.Wrapf(err, "mkdir error for %q", dbPath)
	}

	opts.Dir = dbPath
	opts.ValueDir = opts.Dir // we have small values in this one

	db, err := badger.Open(opts)
	if err != nil {
		return nil, nil, nil, errors.Wrap(err, "db/idx: badger failed to open")
	}

	mlog := multibadger.New(db, msgpack.New(margaret.BaseSeq(0)))

	statePath := r.GetPath("sublogs", name, "state.json")
	idxStateFile, err := os.OpenFile(statePath, os.O_CREATE|os.O_RDWR, 0700)
	if err != nil {
		return nil, nil, nil, errors.Wrap(err, "error opening state file")
	}

	mlogSink := multilog.NewSink(idxStateFile, mlog, f)

	serve := func(ctx context.Context, rootLog margaret.Log) error {
		src, err := rootLog.Query(margaret.Live(true), margaret.SeqWrap(true), mlogSink.QuerySpec())
		if err != nil {
			return errors.Wrap(err, "error querying rootLog for mlog")
		}

		err = luigi.Pump(ctx, mlogSink, src)
		if err == context.Canceled {
			return nil
		}

		return errors.Wrap(err, "error reading query for mlog")
	}

	return mlog, db, serve, nil
}

func GetIndex(r Interface, name string, f func(librarian.Index) librarian.SinkIndex) (librarian.Index, *badger.DB, func(context.Context, margaret.Log) error, error) {
	pth := r.GetPath("indexes", name, "db")
	err := os.MkdirAll(pth, 0700)
	if err != nil {
		return nil, nil, nil, errors.Wrap(err, "error making index directory")
	}

	opts := badger.DefaultOptions
	opts.Dir = pth
	opts.ValueDir = opts.Dir

	db, err := badger.Open(opts)
	if err != nil {
		return nil, nil, nil, errors.Wrap(err, "db/idx: badger failed to open")
	}

	idx := libbadger.NewIndex(db, 0)
	sinkidx := f(idx)

	serve := func(ctx context.Context, rootLog margaret.Log) error {
		src, err := rootLog.Query(margaret.Live(true), margaret.SeqWrap(true), sinkidx.QuerySpec())
		if err != nil {
			return errors.Wrap(err, "error querying root log")
		}

		err = luigi.Pump(ctx, sinkidx, src)
		if err == nil || err == context.Canceled {
			return nil
		}

		return errors.Wrap(err, "contacts index pump failed")
	}

	return idx, db, serve, nil
}

func GetBadgerIndex(r Interface, name string, f func(*badger.DB) librarian.SinkIndex) (*badger.DB, librarian.SinkIndex, func(context.Context, margaret.Log) error, error) {
	pth := r.GetPath("indexes", name, "db")
	err := os.MkdirAll(pth, 0700)
	if err != nil {
		return nil, nil, nil, errors.Wrap(err, "error making index directory")
	}

	opts := badger.DefaultOptions
	opts.Dir = pth
	opts.ValueDir = opts.Dir

	db, err := badger.Open(opts)
	if err != nil {
		return nil, nil, nil, errors.Wrap(err, "db/idx: badger failed to open")
	}

	sinkidx := f(db)

	serve := func(ctx context.Context, rootLog margaret.Log) error {
		src, err := rootLog.Query(margaret.Live(true), margaret.SeqWrap(true), sinkidx.QuerySpec())
		if err != nil {
			return errors.Wrap(err, "error querying root log")
		}

		err = luigi.Pump(ctx, sinkidx, src)
		if err == nil || err == context.Canceled {
			return nil
		}

		return errors.Wrap(err, "contacts index pump failed")
	}

	return db, sinkidx, serve, nil
}
