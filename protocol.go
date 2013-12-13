package main

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"strconv"
	"sync"

	"github.com/angryfoxsu/levigo"
	"github.com/angryfoxsu/bconv"
)

type client struct {
	cn net.Conn
	r  *bufio.Reader
	w  chan []byte

	writeQueueSize int // current queue size in bytes
}

var (
	clients    = make(map[string]*client) // ip:port -> client mapping
	clientsMtx = &sync.RWMutex{}
)

func listen() {
	l, err := net.Listen("tcp", ":12345")
	maybeFatal(err)
	for {
		conn, err := l.Accept()
		if err != nil {
			// TODO: log error
			continue
		}
		go handleClient(conn)
	}
}

func handleClient(cn net.Conn) {
	c := &client{cn: cn, r: bufio.NewReader(cn), w: make(chan []byte)}
	defer close(c.w)

	addr := cn.RemoteAddr().String()
	clientsMtx.Lock()
	clients[addr] = c
	clientsMtx.Unlock()
	defer func() {
		clientsMtx.Lock()
		delete(clients, addr)
		clientsMtx.Unlock()
	}()

	o := make(chan []byte)
	go responseQueue(c, o)
	go responseWriter(c, o)

	protocolHandler(c)
}

func protocolHandler(c *client) {
	// Read a length (looks like "$3\r\n")
	readLength := func(prefix byte) (length int, err error) {
		b, err := c.r.ReadByte()
		if err != nil {
			return
		}
		if b != prefix {
			writeProtocolError(c.w, "invalid length")
			return
		}
		l, overflowed, err := c.r.ReadLine() // Read bytes will look like "123"
		if err != nil {
			return
		}
		if overflowed {
			writeProtocolError(c.w, "length line too long")
			return
		}
		if len(l) == 0 {
			writeProtocolError(c.w, "missing length")
			return
		}
		length, err = bconv.Atoi(l)
		if err != nil {
			writeProtocolError(c.w, "length is not a valid integer")
			return
		}
		return
	}

	runCommand := func(args [][]byte) (err error) {
		if len(args) == 0 {
			writeProtocolError(c.w, "missing command")
			return
		}

		// lookup the command
		command, ok := commands[UnsafeBytesToString(bytes.ToLower(args[0]))]
		if !ok {
			writeError(c.w, "unknown command '"+string(args[0])+"'")
			return
		}

		// check command arity, negative arity means >= n
		if (command.arity < 0 && len(args)-1 < -command.arity) || (command.arity >= 0 && len(args)-1 > command.arity) {
			writeError(c.w, "wrong number of arguments for '"+string(args[0])+"' command")
			return
		}

		// call the command and respond
		var wb *levigo.WriteBatch
		if command.writes {
			wb = levigo.NewWriteBatch()
			defer wb.Close()
		}
		command.lockKeys(args[1:])
		res := command.function(args[1:], wb)
		if command.writes {
			if _, ok := res.(error); !ok { // only write the batch if the return value is not an error
				err = DB.Write(DefaultWriteOptions, wb)
			}
			if err != nil {
				writeError(c.w, "data write error: "+err.Error())
				return
			}
		}
		command.unlockKeys(args[1:])
		writeReply(c.w, res)

		return
	}

	processInline := func() error {
		line, err := c.r.ReadBytes('\n')
		if err != nil {
			return err
		}
		return runCommand(bytes.Split(line[:len(line)-2], []byte(" ")))
	}

	scratch := make([]byte, 2)
	args := [][]byte{}
	// Client event loop, each iteration handles a command
	for {
		// check if we're using the old inline protocol
		b, err := c.r.Peek(1)
		if err != nil {
			return
		}
		if b[0] != '*' {
			err = processInline()
			if err != nil {
				return
			}
			continue
		}

		// Step 1: get the number of arguments
		argCount, err := readLength('*')
		if err != nil {
			return
		}

		// read the arguments
		for i := 0; i < argCount; i++ {
			length, err := readLength('$')
			if err != nil {
				return
			}

			// Read the argument bytes
			args = append(args, make([]byte, length))
			_, err = io.ReadFull(c.r, args[i])
			if err != nil {
				return
			}

			// The argument has a trailing \r\n that we need to discard
			c.r.Read(scratch) // TODO: make sure these bytes are read
		}

		err = runCommand(args)
		if err != nil {
			return
		}

		// Truncate arguments for the next run
		args = args[:0]
	}
}

func responseQueue(c *client, out chan<- []byte) {
	defer close(out)

	queue := [][]byte{}

receive:
	for {
		// ensure that the queue always has an item in it
		if len(queue) == 0 {
			v, ok := <-c.w
			if !ok {
				break // in is closed, we're done
			}
			queue = append(queue, v)
			c.writeQueueSize += len(v)
		}

		select {
		case out <- queue[0]:
			c.writeQueueSize -= len(queue[0])
			queue = queue[1:]
		case v, ok := <-c.w:
			if !ok {
				break receive // in is closed, we're done
			}
			queue = append(queue, v)
			c.writeQueueSize += len(v)
		}
	}

	for _, v := range queue {
		out <- v
	}
}

func responseWriter(c *client, out <-chan []byte) {
	for v := range out {
		c.cn.Write(v)
	}
}

func writeReply(w chan<- []byte, reply interface{}) {
	if _, ok := reply.([]interface{}); !ok && reply == nil {
		w <- []byte("$-1\r\n")
		return
	}
	switch r := reply.(type) {
	case rawReply:
		w <- r
	case string:
		w <- []byte("+" + r + "\r\n")
	case []byte:
		writeBulk(w, r)
	case int:
		writeInt(w, int64(r))
	case int64:
		writeInt(w, r)
	case uint32:
		writeInt(w, int64(r))
	case IOError:
		w <- []byte("-IOERR " + reply.(IOError).Error() + "\r\n")
	case error:
		writeError(w, r.Error())
	case []interface{}:
		writeMultibulk(w, r)
	case *cmdReplyStream:
		writeMultibulkStream(w, r)
	case map[string]bool:
		writeMultibulkStringMap(w, r)
	default:
		panic("Invalid reply type")
	}
}

func writeProtocolError(w chan<- []byte, msg string) {
	writeError(w, "Protocol error: "+msg)
}

func writeInt(w chan<- []byte, n int64) {
	w <- append(strconv.AppendInt([]byte{':'}, n, 10), "\r\n"...)
}

func writeBulk(w chan<- []byte, b []byte) {
	if b == nil {
		w <- []byte("$-1\r\n")
	}
	// TODO: find a more efficient way of doing this
	w <- append(strconv.AppendInt([]byte{'$'}, int64(len(b)), 10), "\r\n"...)
	w <- b
	w <- []byte("\r\n")
}

func writeMultibulkStream(w chan<- []byte, reply *cmdReplyStream) {
	writeMultibulkLength(w, reply.size)
	for r := range reply.items {
		writeReply(w, r)
	}
}

func writeMultibulk(w chan<- []byte, reply []interface{}) {
	if reply == nil {
		writeMultibulkLength(w, -1)
	}
	writeMultibulkLength(w, int64(len(reply)))
	for _, r := range reply {
		writeReply(w, r)
	}
}

func writeMultibulkStringMap(w chan<- []byte, reply map[string]bool) {
	writeMultibulkLength(w, int64(len(reply)))
	for r, _ := range reply {
		writeBulk(w, []byte(r))
	}
}

func writeMultibulkLength(w chan<- []byte, n int64) {
	w <- append(strconv.AppendInt([]byte{'*'}, n, 10), "\r\n"...)
}

func writeError(w chan<- []byte, msg string) {
	w <- []byte("-ERR " + msg + "\r\n")
}
