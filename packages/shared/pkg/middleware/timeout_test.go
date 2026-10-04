package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequestTimeout_SetsDeadline(t *testing.T) {
	t.Parallel()

	r := gin.New()
	r.Use(RequestTimeout(500 * time.Millisecond))
	r.GET("/test", func(c *gin.Context) {
		deadline, ok := c.Request.Context().Deadline()
		assert.True(t, ok, "context should have a deadline")
		assert.WithinDuration(t, time.Now().Add(500*time.Millisecond), deadline, 100*time.Millisecond)
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "/test", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
}

func TestRequestTimeout_CancelsBlockingHandler(t *testing.T) {
	t.Parallel()

	r := gin.New()
	r.Use(RequestTimeout(100 * time.Millisecond))

	handlerDone := make(chan struct{})
	r.GET("/slow", func(c *gin.Context) {
		defer close(handlerDone)
		<-c.Request.Context().Done()
		c.Status(http.StatusServiceUnavailable)
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/slow", nil)

	start := time.Now()
	r.ServeHTTP(w, req)
	elapsed := time.Since(start)

	<-handlerDone
	assert.Less(t, elapsed, 500*time.Millisecond, "handler should have been unblocked by context timeout")
}

func TestRequestTimeout_NormalRequestContextNotCanceled(t *testing.T) {
	t.Parallel()

	// Simulate an outer middleware that reads CancelCause after c.Next().
	// The context itself will be canceled by defer cancel(), but CancelCause
	// should return nil for normal (non-timed-out) requests.
	var outerCause error
	outerMiddleware := func(c *gin.Context) {
		c.Next()
		outerCause = CancelCause(c)
	}

	r := gin.New()
	r.Use(outerMiddleware)
	r.Use(RequestTimeout(5 * time.Second))
	r.GET("/test", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/test", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.NoError(t, outerCause, "CancelCause should return nil for normal requests")
}

func TestRequestTimeout_TimeoutContextVisibleToOuterMiddleware(t *testing.T) {
	t.Parallel()

	var outerCause error
	outerMiddleware := func(c *gin.Context) {
		c.Next()
		outerCause = CancelCause(c)
	}

	r := gin.New()
	r.Use(outerMiddleware)
	r.Use(RequestTimeout(50 * time.Millisecond))
	r.GET("/slow", func(c *gin.Context) {
		<-c.Request.Context().Done()
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/slow", nil)
	r.ServeHTTP(w, req)

	require.ErrorIs(t, outerCause, ErrRequestTimeout,
		"outer middleware should see ErrRequestTimeout as the cause when the timeout fires")
}

func TestRequestTimeout_RouteOverrideAppliesToItsPatternOnly(t *testing.T) {
	t.Parallel()

	r := gin.New()
	r.Use(RequestTimeout(500*time.Millisecond, RouteTimeout{Route: "/items/:id/slow", Timeout: time.Hour}))

	deadlines := map[string]time.Time{}
	record := func(c *gin.Context) {
		deadline, ok := c.Request.Context().Deadline()
		assert.True(t, ok, "context should have a deadline")
		deadlines[c.FullPath()] = deadline
		c.Status(http.StatusOK)
	}
	r.GET("/items/:id/slow", record)
	r.GET("/items/:id", record)

	for _, path := range []string{"/items/abc/slow", "/items/abc"} {
		w := httptest.NewRecorder()
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
		r.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)
	}

	assert.WithinDuration(t, time.Now().Add(time.Hour), deadlines["/items/:id/slow"], time.Second)
	assert.WithinDuration(t, time.Now().Add(500*time.Millisecond), deadlines["/items/:id"], 200*time.Millisecond)
}

// The server's WriteTimeout would cut the connection before a long handler on
// an overridden route answers; the override pushes the write deadline out.
func TestRequestTimeout_RouteOverrideOutlivesServerWriteTimeout(t *testing.T) {
	t.Parallel()

	r := gin.New()
	r.Use(RequestTimeout(50*time.Millisecond, RouteTimeout{Route: "/slow", Timeout: 5 * time.Second}))
	handler := func(c *gin.Context) {
		time.Sleep(300 * time.Millisecond)
		c.Status(http.StatusNoContent)
	}
	r.POST("/slow", handler)
	r.POST("/default", handler)

	srv := httptest.NewUnstartedServer(r)
	srv.Config.WriteTimeout = 100 * time.Millisecond
	srv.Start()
	t.Cleanup(srv.Close)

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/slow", nil)
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)

	req, _ = http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/default", nil)
	resp, err = srv.Client().Do(req)
	if err == nil {
		resp.Body.Close()
	}
	require.Error(t, err, "without an override the server's WriteTimeout cuts the connection")
}
