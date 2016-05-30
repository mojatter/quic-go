package h2quic

import (
	"errors"
	"io/ioutil"
	"net/http"
	"time"

	"github.com/lucas-clemente/quic-go"
	"github.com/lucas-clemente/quic-go/protocol"
	"github.com/lucas-clemente/quic-go/utils"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

type streamCreator interface {
	GetOrOpenStream(protocol.StreamID) (utils.Stream, error)
	Close(error) error
}

// Server is a HTTP2 server listening for QUIC connections.
// The nil value is invalid, as a valid TLS config is required.
type Server struct {
	*http.Server

	// Private flag for demo, do not use
	CloseAfterFirstRequest bool
}

// ListenAndServe listens on the network address and calls the handler.
func (s *Server) ListenAndServe() error {
	if s.Server == nil {
		return errors.New("use of h2quic.Server without http.Server")
	}

	server, err := quic.NewServer(s.Addr, s.TLSConfig, s.handleStreamCb)
	if err != nil {
		return err
	}

	return server.ListenAndServe()
}

func (s *Server) handleStreamCb(session *quic.Session, stream utils.Stream) {
	s.handleStream(session, stream)
}

func (s *Server) handleStream(session streamCreator, stream utils.Stream) {
	if stream.StreamID() != 3 {
		return
	}

	hpackDecoder := hpack.NewDecoder(4096, nil)
	h2framer := http2.NewFramer(nil, stream)

	go func() {
		for {
			if err := s.handleRequest(session, stream, hpackDecoder, h2framer); err != nil {
				utils.Errorf("error handling h2 request: %s", err.Error())
				return
			}
		}
	}()
}

func (s *Server) handleRequest(session streamCreator, headerStream utils.Stream, hpackDecoder *hpack.Decoder, h2framer *http2.Framer) error {
	h2frame, err := h2framer.ReadFrame()
	if err != nil {
		return err
	}
	h2headersFrame := h2frame.(*http2.HeadersFrame)
	if !h2headersFrame.HeadersEnded() {
		return errors.New("http2 header continuation not implemented")
	}
	headers, err := hpackDecoder.DecodeFull(h2headersFrame.HeaderBlockFragment())
	if err != nil {
		utils.Errorf("invalid http2 headers encoding: %s", err.Error())
		return err
	}

	req, err := requestFromHeaders(headers)
	if err != nil {
		return err
	}
	utils.Infof("%s %s%s", req.Method, req.Host, req.RequestURI)

	dataStream, err := session.GetOrOpenStream(protocol.StreamID(h2headersFrame.StreamID))
	if err != nil {
		return err
	}

	if h2headersFrame.StreamEnded() {
		dataStream.CloseRemote(0)
	}

	// stream's Close() closes the write side, not the read side
	req.Body = ioutil.NopCloser(dataStream)

	responseWriter := newResponseWriter(headerStream, dataStream, protocol.StreamID(h2headersFrame.StreamID))

	go func() {
		handler := s.Handler
		if handler == nil {
			handler = http.DefaultServeMux
		}
		handler.ServeHTTP(responseWriter, req)
		if responseWriter.dataStream != nil {
			responseWriter.dataStream.Close()
		}
		if s.CloseAfterFirstRequest {
			time.Sleep(100 * time.Millisecond)
			session.Close(nil)
		}
	}()

	return nil
}
