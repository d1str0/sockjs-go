package sockjs

import (
	"io"
	"net/http"
	"net/http/httputil"
)

type protocol interface {
	contentType() string
	writeOpen(io.Writer) error
	writeData(io.Writer, ...[]byte) (int, error)
	writeClose(io.Writer, int, string)
}

type streamingProtocol interface {
	protocol
	writePrelude(io.Writer) error
}

func pollingHandler(h *Handler,
	w http.ResponseWriter,
	r *http.Request,
	sessid string,
	proto protocol) {
	var err error
	header := w.Header()
	header.Add("Content-Type", proto.contentType())
	disableCache(header)
	preflight(header, r)

	s, exists := h.pool.getOrCreate(sessid)
	if !exists {
		// initiate connection
		if err = proto.writeOpen(w); err != nil {
			h.pool.remove(sessid)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		go h.hfunc(s)
		return
	}

	fail := s.reserve()
	if fail {
		proto.writeClose(w, 2010, "Another connection still open")
		return
	}
	defer s.free()

	m, err := s.out.pullAll()
	if err != nil {
		proto.writeClose(w, 3000, "Go away!")
		return
	}
	proto.writeData(w, m...)
}

func streamingHandler(h *Handler,
	w http.ResponseWriter,
	r *http.Request,
	sessid string,
	proto streamingProtocol) {
	header := w.Header()
	header.Add("Content-Type", proto.contentType())
	disableCache(header)
	preflight(header, r)
	w.WriteHeader(http.StatusOK)

	conn, bufrw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer conn.Close()

	chunkedw := httputil.NewChunkedWriter(bufrw)
	defer func() {
		chunkedw.Close()
		bufrw.Write([]byte("\r\n")) // close for chunked data
		bufrw.Flush()
	}()

	if err = proto.writePrelude(chunkedw); err != nil {
		return
	}
	if err = bufrw.Flush(); err != nil {
		return
	}

	s, exists := h.pool.getOrCreate(sessid)
	if !exists {
		// initiate connection
		if err = proto.writeOpen(chunkedw); err != nil {
			goto fail
		}
		if err = bufrw.Flush(); err != nil {
			goto fail
		}
		goto success
	fail:
		h.pool.remove(sessid)
		return
	success:
		go h.hfunc(s)
	}

	fail := s.reserve()
	if fail {
		proto.writeClose(chunkedw, 2010, "Another connection still open")
		bufrw.Flush()
		return
	}
	defer s.free()

	for sent := 0; sent < h.config.ResponseLimit; {
		m, err := s.out.pullAll()
		if err != nil {
			proto.writeClose(chunkedw, 3000, "Go away!")
			bufrw.Flush()
			return
		}

		n, err := proto.writeData(chunkedw, m...)
		if err != nil {
			return
		}
		if err = bufrw.Flush(); err != nil {
			return
		}
		sent += n
	}
}
