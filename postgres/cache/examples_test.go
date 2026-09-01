package cache_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/alextanhongpin/dbtx/postgres/cache"
	"github.com/alextanhongpin/dbtx/postgres/lock"
	"github.com/alextanhongpin/dbtx/testing/dbtest"
	"github.com/stretchr/testify/assert"
)

var ErrNegativeCacheHit = errors.New("negative cache hit")

func TestCache(t *testing.T) {
	ctx := t.Context()
	repo := NewBookRepository(dbtest.DB(t))
	repo.Cache.SetPrefix("books:")

	t.Run("empty", func(t *testing.T) {
		// Given that the db does not have the data.
		// When calling find
		id := uuid.NewV7()
		b, loaded, err := repo.Find(ctx, id)

		is := assert.New(t)
		is.Nil(b)
		is.False(loaded)
		is.ErrorIs(err, ErrNegativeCacheHit)

		// Then it should cache negative hit.
		for range 3 {
			b, loaded, err := repo.Find(ctx, id)
			is.Nil(b)
			is.True(loaded)
			is.ErrorIs(err, ErrNegativeCacheHit)
		}
	})

	t.Run("create", func(t *testing.T) {
		// Given that cache is empty
		// When creating
		b, err := repo.Create(ctx, t.Name())

		// Then it should return a book.
		is := assert.New(t)
		is.NoError(err)
		is.NotEmpty(b.ID)
		is.Equal(b.Title, t.Name())

		// And it should be cached.
		exists, err := repo.Cache.Exists(ctx, b.ID.String())
		is.NoError(err)
		is.True(exists)

		old := b

		t.Run("exists", func(t *testing.T) {
			// When the cache has data.
			b, loaded, err := repo.Find(ctx, old.ID)
			is := assert.New(t)
			// Then it should return the data from cache.
			is.NoError(err)
			is.Equal(old.ID, b.ID)
			is.Equal(old.Title, b.Title)
			is.True(loaded)
		})

		t.Run("cache expired", func(t *testing.T) {
			err := repo.Cache.Delete(ctx, old.ID.String())
			is := assert.New(t)
			is.NoError(err)

			// When the cache has expired.
			b, loaded, err := repo.Find(ctx, old.ID)

			// Then it should fetch the data from db
			// And set the cache.
			is.Equal(old.ID, b.ID)
			is.Equal(old.Title, b.Title)
			is.False(loaded)
			is.NoError(err)
		})

		t.Run("deleted", func(t *testing.T) {
			is := assert.New(t)
			is.NoError(err)

			// When the cache has expired.
			b, err := repo.Delete(ctx, old.ID)
			is.NoError(err)
			is.Equal(old.ID, b.ID)
			is.Equal(old.Title, b.Title)

			exists, err := repo.Cache.Exists(ctx, old.ID.String())
			is.NoError(err)
			is.False(exists)
		})
	})
}

type Book struct {
	ID    uuid.UUID
	Title string
}

type BookRepository struct {
	*cache.Cache
}

func NewBookRepository(db *sql.DB) *BookRepository {
	return &BookRepository{
		Cache: cache.New(db),
	}
}

func (r *BookRepository) Create(ctx context.Context, title string) (*Book, error) {
	return r.DB.RunInTx2(ctx, func(ctx context.Context) (*Book, error) {
		var id uuid.UUID
		err := r.DBTx(ctx).QueryRowContext(ctx, `insert into books (title) values ($1) returning id`, title).Scan(&id)
		if err != nil {
			return nil, err
		}
		b := &Book{ID: id, Title: title}

		// Cached atomically in transaction upon creation.
		err = r.Store(ctx, b.ID.String(), b, time.Second)
		if err != nil {
			return nil, err
		}
		return b, nil
	})
}

func (r *BookRepository) Find(ctx context.Context, id uuid.UUID) (*Book, bool, error) {
	key := id.String()
	b, err := r.Load[*Book](ctx, key)
	if err == nil {
		if b.ID == uuid.Nil() {
			return nil, true, ErrNegativeCacheHit
		}
		return b, true, nil
	}

	b, err = r.RunInTx2(ctx, func(ctx context.Context) (*Book, error) {
		// No longer need singleflight, only one transaction will have access to this.
		if err := lock.Lock(ctx, lock.NewStrKey(key)); err != nil {
			return nil, err
		}

		var title string
		err = r.DBTx(ctx).QueryRowContext(ctx, `select title from books where id = $1`, id).Scan(&title)
		if errors.Is(err, sql.ErrNoRows) {
			// Store negative cache to avoid thundering herd.
			b := new(Book)
			if err := r.Store(ctx, key, b, 10*time.Second); err != nil {
				return nil, err
			}
			// Don't return error here, since r.Store runs in transaction.
			return b, nil
		}
		if err != nil {
			return nil, err
		}

		// Store in transaction!
		b := &Book{ID: id, Title: title}
		err = r.Store(ctx, key, b, time.Second)
		if err != nil {
			return nil, err
		}
		return b, nil
	})
	if err != nil {
		return nil, false, err
	}
	if b.ID == uuid.Nil() {
		return nil, false, ErrNegativeCacheHit
	}
	return b, false, nil
}

func (r *BookRepository) Delete(ctx context.Context, id uuid.UUID) (*Book, error) {
	key := id.String()
	return r.RunInTx2(ctx, func(ctx context.Context) (*Book, error) {
		var title string
		err := r.DBTx(ctx).QueryRowContext(ctx, `delete from books where id = $1 returning title`, id).Scan(&title)
		if err != nil {
			return nil, err
		}
		b := &Book{
			ID:    id,
			Title: title,
		}

		// Cache deleted atomically.
		err = r.Cache.Delete(ctx, key)
		if err != nil {
			return nil, err
		}
		return b, nil
	})
}
