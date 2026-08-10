// Command postghost serves a small message board backed by Postgres.
package main

import (
	"context"
	"embed"
	"html/template"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed templates/*.html static/*
var assets embed.FS

const schema = `
CREATE TABLE IF NOT EXISTS posts (
	id         BIGSERIAL PRIMARY KEY,
	author     TEXT        NOT NULL,
	body       TEXT        NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS posts_created_at_idx ON posts (created_at DESC);
`

// Post is one message on the board.
type Post struct {
	ID        int64
	Author    string
	Body      string
	CreatedAt time.Time
}

type server struct {
	pool  *pgxpool.Pool
	tmpl  *template.Template
	files http.Handler
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("postghost: ")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	addr := ":" + env("PORT", "8080")

	pool, err := connect(ctx, dsn())
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx, schema); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	srv := &server{
		pool:  pool,
		tmpl:  template.Must(template.ParseFS(assets, "templates/*.html")),
		files: http.FileServer(http.FS(assets)),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", srv.list)
	mux.HandleFunc("POST /posts", srv.create)
	mux.HandleFunc("POST /posts/{id}/delete", srv.delete)
	mux.HandleFunc("GET /healthz", srv.health)
	mux.Handle("GET /static/", srv.files)

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           logRequests(mux),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		log.Printf("listening on %s", addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	<-ctx.Done()
	log.Print("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

// logRequests writes one line per request with its method, path and duration.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

// env returns the value of key, or def when unset or empty.
func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// dsn builds the Postgres connection string. DB_URL wins when set; otherwise
// the URL is assembled from the discrete DB_* variables.
func dsn() string {
	if u := os.Getenv("DB_URL"); u != "" {
		return u
	}
	host := env("DB_HOST", "localhost")
	port := env("DB_PORT", "5432")
	name := env("DB_NAME", "postghost")
	user := env("DB_USER", "postghost")
	pass := env("DB_PASS", "postghost")

	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(user, pass),
		Host:   net.JoinHostPort(host, port),
		Path:   "/" + name,
	}
	// Managed Postgres usually requires TLS; plain hosts (Compose, localhost)
	// usually don't ship a certificate. sslmode=prefer covers both.
	u.RawQuery = url.Values{"sslmode": {env("DB_SSLMODE", "prefer")}}.Encode()
	return u.String()
}

// connect dials Postgres, retrying until the context is done so the app can
// start alongside its database under Compose.
func connect(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	for attempt := 1; ; attempt++ {
		if err := pool.Ping(ctx); err == nil {
			return pool, nil
		} else if attempt == 30 {
			pool.Close()
			return nil, err
		}
		log.Printf("waiting for database (attempt %d)", attempt)
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (s *server) list(w http.ResponseWriter, r *http.Request) {
	posts, err := s.posts(r.Context())
	if err != nil {
		s.fail(w, "load posts", err)
		return
	}
	s.render(w, posts)
}

func (s *server) create(w http.ResponseWriter, r *http.Request) {
	author := trim(r.FormValue("author"), 40)
	body := trim(r.FormValue("body"), 500)
	if author == "" {
		author = "anonymous"
	}
	if body == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	_, err := s.pool.Exec(r.Context(),
		`INSERT INTO posts (author, body) VALUES ($1, $2)`, author, body)
	if err != nil {
		s.fail(w, "insert post", err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *server) delete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad post id", http.StatusBadRequest)
		return
	}
	if _, err := s.pool.Exec(r.Context(), `DELETE FROM posts WHERE id = $1`, id); err != nil {
		s.fail(w, "delete post", err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	if err := s.pool.Ping(r.Context()); err != nil {
		http.Error(w, "unhealthy", http.StatusServiceUnavailable)
		return
	}
	w.Write([]byte("ok"))
}

func (s *server) posts(ctx context.Context) ([]Post, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, author, body, created_at FROM posts ORDER BY created_at DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var posts []Post
	for rows.Next() {
		var p Post
		if err := rows.Scan(&p.ID, &p.Author, &p.Body, &p.CreatedAt); err != nil {
			return nil, err
		}
		posts = append(posts, p)
	}
	return posts, rows.Err()
}

func (s *server) render(w http.ResponseWriter, posts []Post) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "index.html", posts); err != nil {
		log.Printf("render: %v", err)
	}
}

func (s *server) fail(w http.ResponseWriter, what string, err error) {
	log.Printf("%s: %v", what, err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

// trim strips surrounding whitespace and caps the result at max runes.
func trim(s string, max int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > max {
		r = r[:max]
	}
	return string(r)
}
