package channel

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// DefaultMaxEventBytes 是单条 SSE 事件负载的默认上限。上游返回超长事件时按错误
// 处理，避免无上限地吃内存。
const DefaultMaxEventBytes = 8 << 20

// copyBufferBytes 是逐段搬运上游流时的缓冲区大小。
const copyBufferBytes = 32 << 10

// ErrEventTooLarge 表示单条事件超过上限。
var ErrEventTooLarge = errors.New("channel: 事件负载超过上限")

// Event 是一条 SSE 事件。
type Event struct {
	// Name 是 event 字段，缺省为空。
	Name string
	// Data 是 data 字段拼接后的内容，末尾换行已去掉。
	Data []byte
}

// Reader 逐个读出 SSE 事件。
type Reader struct {
	reader        *bufio.Reader
	maxEventBytes int
}

// NewReader 创建事件阅读器。
func NewReader(reader io.Reader) *Reader {
	return &Reader{reader: bufio.NewReader(reader), maxEventBytes: DefaultMaxEventBytes}
}

// SetMaxEventBytes 调整单条事件上限，非正数表示用默认值。
func (r *Reader) SetMaxEventBytes(limit int) {
	if limit > 0 {
		r.maxEventBytes = limit
	}
}

// Next 返回下一条事件。流结束时返回 io.EOF。
//
// 解析遵循 SSE 的常用子集：忽略注释行与未知字段；data 行按行拼接，行间补 \n；
// 空行分发事件；`data: [DONE]` 之类的结束标记原样返回，由调用方判断。
func (r *Reader) Next() (Event, error) {
	var (
		event Event
		data  bytes.Buffer
	)
	for {
		line, err := r.readLine()
		if err != nil {
			if errors.Is(err, io.EOF) && (data.Len() > 0 || event.Name != "") {
				return finishEvent(event, data), nil
			}
			return Event{}, err
		}
		if line == "" {
			if data.Len() == 0 && event.Name == "" {
				continue
			}
			return finishEvent(event, data), nil
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			field, value = line, ""
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event.Name = value
		case "data":
			if data.Len()+len(value) > r.maxEventBytes {
				return Event{}, fmt.Errorf("%w: 已超过 %d 字节", ErrEventTooLarge, r.maxEventBytes)
			}
			data.WriteString(value)
			data.WriteByte('\n')
		default:
			// id、retry 与其它字段不参与内容，忽略。
		}
	}
}

// readLine 读一行并去掉行尾的 CR/LF。
//
// 用 ReadSlice 逐段拼，而不是 ReadString：ReadString 会把整行先读进内存再返回，
// 上游发一条不含换行的超长行就能把内存吃满。这里每读一段都对照上限。
func (r *Reader) readLine() (string, error) {
	var line []byte
	for {
		chunk, err := r.reader.ReadSlice('\n')
		line = append(line, chunk...)
		switch {
		case err == nil:
			return trimLineEnding(line), nil
		case errors.Is(err, bufio.ErrBufferFull):
			if len(line) > r.maxEventBytes {
				return "", fmt.Errorf("%w: 单行超过 %d 字节", ErrEventTooLarge, r.maxEventBytes)
			}
		default:
			return trimLineEnding(line), err
		}
	}
}

// trimLineEnding 去掉行尾的 CR/LF。
func trimLineEnding(line []byte) string {
	return strings.TrimRight(string(line), "\r\n")
}

// finishEvent 收尾一条事件：去掉拼接时补的最后一个换行。
func finishEvent(event Event, data bytes.Buffer) Event {
	payload := data.Bytes()
	if len(payload) > 0 && payload[len(payload)-1] == '\n' {
		payload = payload[:len(payload)-1]
	}
	return Event{Name: event.Name, Data: payload}
}

// Writer 按 SSE 格式写出事件，并在每次写出后刷新。
type Writer struct {
	buffer  *bufio.Writer
	flusher http.Flusher
}

// NewWriter 创建事件写出器。底层实现 http.Flusher 时会自动刷新。
func NewWriter(writer io.Writer) *Writer {
	flusher, _ := writer.(http.Flusher)
	return &Writer{buffer: bufio.NewWriter(writer), flusher: flusher}
}

// Write 写出一条事件并刷新。
func (w *Writer) Write(event Event) error {
	if event.Name != "" {
		if _, err := fmt.Fprintf(w.buffer, "event: %s\n", event.Name); err != nil {
			return err
		}
	}
	if err := writeDataLines(w.buffer, event.Data); err != nil {
		return err
	}
	if _, err := w.buffer.WriteString("\n"); err != nil {
		return err
	}
	return w.Flush()
}

// WriteRaw 原样写出一行 data 字段（不拆行），用于 [DONE] 之类的标记。
func (w *Writer) WriteRaw(data []byte) error {
	if _, err := w.buffer.WriteString("data: "); err != nil {
		return err
	}
	if _, err := w.buffer.Write(data); err != nil {
		return err
	}
	if _, err := w.buffer.WriteString("\n\n"); err != nil {
		return err
	}
	return w.Flush()
}

// Flush 把缓冲写到底层并调用其 Flush。
func (w *Writer) Flush() error {
	if err := w.buffer.Flush(); err != nil {
		return err
	}
	if w.flusher != nil {
		w.flusher.Flush()
	}
	return nil
}

// writeDataLines 按行写 data 字段。
func writeDataLines(writer *bufio.Writer, data []byte) error {
	for _, line := range strings.Split(string(data), "\n") {
		if _, err := fmt.Fprintf(writer, "data: %s\n", line); err != nil {
			return err
		}
	}
	return nil
}

// CopyRaw 把上游流逐段搬给下游，不做解析，保留原始字节。返回搬运的字节数。
//
// flushEvery 为每次刷新前累计的字节阈值，0 表示每段都刷新。同方言转发走这条路，
// 未建模字段因此逐字节保留。
func CopyRaw(dst io.Writer, src io.Reader, flush func()) (int64, error) {
	flusher := &flushWriter{writer: dst, flush: flush}
	buffer := make([]byte, copyBufferBytes)
	var total int64
	for {
		read, err := src.Read(buffer)
		if read > 0 {
			written, writeErr := flusher.Write(buffer[:read])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return total, nil
			}
			return total, err
		}
	}
}

// flushWriter 每次写入后都刷新。
type flushWriter struct {
	writer io.Writer
	flush  func()
}

func (w *flushWriter) Write(data []byte) (int, error) {
	written, err := w.writer.Write(data)
	if w.flush != nil {
		w.flush()
	}
	return written, err
}
