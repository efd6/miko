package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sys/execabs"
	"golang.org/x/tools/txtar"
	. "modernc.org/tk9.0"
)

func main() {
	txt := flag.String("txtar", "", "txtar file containing src.cel, data.json and cfg.yaml (incompatible with any other argument)")
	srcPath := flag.String("src", "", "path to a CEL program")
	dataPath := flag.String("data", "", "path to a JSON object holding input (exposed as the label state)")
	cfgPath := flag.String("cfg", "", "path to a YAML file holding run control configuration (see pkg.go.dev/github.com/elastic/mito/cmd/mito)")
	font := flag.String("font", "Courier", "font family")
	size := flag.Uint("face_size", 10, "font face size")
	tw := flag.Uint("tw", 4, "width of tab stops measured in spaces")
	flag.Parse()
	if *txt != "" && (*dataPath != "" || *cfgPath != "" || *srcPath != "") || *tw == 0 {
		flag.Usage()
		os.Exit(2)
	}
	m := newMiko(*font, int(*size), int(*tw))
	if *txt != "" {
		b, err := os.ReadFile(*txt)
		if err != nil {
			log.Fatal(err)
		}
		ar := txtar.Parse(b)
		for _, f := range ar.Files {
			switch f.Name {
			case "src.cel":
				m.src.Insert("end", string(f.Data))
			case "data.json":
				m.data.Insert("end", string(f.Data))
			case "cfg.yaml":
				m.cfg.Insert("end", string(f.Data))
			}
		}
	}
	if *srcPath != "" {
		b, err := os.ReadFile(*srcPath)
		if err != nil {
			log.Fatal(err)
		}
		m.src.Insert("end", string(b))
	}
	if *dataPath != "" {
		b, err := os.ReadFile(*dataPath)
		if err != nil {
			log.Fatal(err)
		}
		m.data.Insert("end", string(b))
	}
	if *cfgPath != "" {
		b, err := os.ReadFile(*cfgPath)
		if err != nil {
			log.Fatal(err)
		}
		m.cfg.Insert("end", string(b))
	}
	m.main()
}

type miko struct {
	ps      atomic.Pointer[os.Process]
	results chan text
	src     *TextWidget
	data    *TextWidget
	cfg     *TextWidget
	display *TextWidget
}

type text struct {
	data string
	tag  string
}

func newMiko(font string, size, tw int) *miko {
	App.WmTitle("miko")
	App.SetResizable(false, false)

	m := &miko{results: make(chan text)}

	buttons := App.Frame()

	run := buttons.Window.Button(
		Txt("Run"),
		Command(func() {
			if ps := m.ps.Load(); ps != nil {
				err := ps.Kill()
				if err != nil {
					m.printError(err)
				}
			}
			ps, err := m.mito(false)
			m.ps.Store(ps)
			if err != nil {
				m.printError(err)
			}
		}),
	)

	format := buttons.Window.Button(
		Txt("Format"),
		Command(func() {
			src, err := m.celfmt()
			if err != nil {
				m.printError(err)
				return
			}
			if src != "" {
				m.src.Clear()
				m.src.Insert("end", src)
			}
			data, err := m.jsonfmt()
			if err != nil {
				m.printError(err)
				return
			}
			if data != "" {
				m.data.Clear()
				m.data.Insert("end", data)
			}
		}),
	)

	cancel := buttons.Window.Button(
		Txt("Cancel"),
		Command(func() {
			if ps := m.ps.Load(); ps != nil {
				err := ps.Kill()
				if err != nil {
					m.printError(err)
				}
			}
		}),
	)

	clear := buttons.Window.Button(
		Txt("Clear Output"),
		Command(func() {
			m.display.Configure(State("normal"))
			m.display.Clear()
			m.display.TagConfigure("output", Foreground("black"))
			m.display.TagConfigure("error", Foreground("red"))
			m.display.Configure(State("disabled"))
		}),
	)

	snarf := buttons.Window.Button(
		Txt("Snarf"),
		Command(func() {
			var ar txtar.Archive
			src := m.src.Text()
			if src != "" {
				ar.Files = append(ar.Files, txtar.File{Name: "src.cel", Data: []byte(src)})
			}
			data := m.data.Text()
			if data != "" {
				ar.Files = append(ar.Files, txtar.File{Name: "data.json", Data: []byte(data)})
			}
			cfg := m.cfg.Text()
			if cfg != "" {
				ar.Files = append(ar.Files, txtar.File{Name: "cfg.yaml", Data: []byte(cfg)})
			}
			out := m.display.Text()
			if out != "" {
				ar.Files = append(ar.Files, txtar.File{Name: "out.json", Data: []byte(out)})
			}
			ClipboardClear()
			ClipboardAppend(string(txtar.Format(&ar)))
		}),
	)

	for i, b := range []*ButtonWidget{
		run,
		cancel,
		format,
		snarf,
		clear,
	} {
		Grid(b, Row(0), Column(i), Sticky("news"))
		GridColumnConfigure(buttons.Window, i, Weight(1))
	}
	Grid(buttons, Row(0), Column(0), Sticky("news"))

	face := NewFont(Family(font), Size(size))
	tabWidth := face.Measure(App, strings.Repeat(" ", tw))

	for i, input := range []struct {
		name  string
		frame *FrameWidget
		text  **TextWidget
	}{
		{name: "src (CEL)", frame: App.Frame(), text: &m.src},
		{name: "data (JSON)", frame: App.Frame(), text: &m.data},
		{name: "cfg (YAML)", frame: App.Frame(), text: &m.cfg},
	} {
		textWidget(input.text, input.frame, input.name, face, tabWidth, true)
		Grid(input.frame, Row(i+1), Column(0), Sticky("news"))
	}

	display := App.Frame()
	textWidget(&m.display, display, "", face, tabWidth, false)
	Grid(display, Row(0), Rowspan(5), Column(1), Sticky("news"))
	GridRowConfigure(display, 1, Weight(1))
	m.display.Configure(State("disabled"))
	m.display.TagConfigure("output", Foreground("black"))
	m.display.TagConfigure("error", Foreground("red"))

	Focus(m.src)

	NewTicker(100*time.Millisecond, func() {
		select {
		case text := <-m.results:
			m.display.Configure(State("normal"))
			m.display.Insert("end", text.data, text.tag)
			m.display.Configure(State("disabled"))
		default:
		}
	})

	return m
}

func textWidget(dst **TextWidget, frame *FrameWidget, title string, face *FontFace, tabWidth int, undo bool) {
	w := frame.Window
	scrollX := w.TScrollbar(Command(func(e *Event) { e.Xview(*dst) }), Orient("horizontal"))
	scrollY := w.TScrollbar(Command(func(e *Event) { e.Yview(*dst) }), Orient("vertical"))
	*dst = w.Text(
		Font(face),
		Width(120),
		Tabs(tabWidth),
		Undo(undo),
		Wrap("none"),
		Setgrid(true),
		Background(White),
		Padx("1m"), Pady("1m"),
		Blockcursor(false),
		Insertunfocussed("hollow"),
		Xscrollcommand(func(e *Event) { e.ScrollSet(scrollX) }),
		Yscrollcommand(func(e *Event) { e.ScrollSet(scrollY) }),
	)
	if title != "" {
		Grid(w.Label(Anchor("w"), Txt(title)), Row(0), Sticky("w"))
	}
	Grid(*dst, Row(1), Column(0), Sticky("news"))
	Grid(scrollY, Row(1), Column(1), Sticky("news"))
	Grid(scrollX, Row(2), Column(0), Sticky("news"))
}

func (m *miko) printError(err error) {
	if err == nil {
		return
	}
	m.display.Configure(State("normal"))
	m.display.Insert("end", err.Error(), "error")
	m.display.Configure(State("disabled"))
}

func (*miko) main() {
	App.Wait()
}

func (m *miko) mito(keep bool) (*os.Process, error) {
	src := m.src.Text()
	if src == "" {
		return nil, nil
	}
	dir, err := os.MkdirTemp("", "miko-*")
	if err != nil {
		return nil, err
	}
	var cmd *execabs.Cmd
	defer func() {
		if cmd == nil {
			os.RemoveAll(dir)
		}
	}()
	var args []string
	data := m.data.Text()
	if data != "" {
		dataPath := filepath.Join(dir, "data.json")
		err = os.WriteFile(dataPath, []byte(data), 0o600)
		if err != nil {
			return nil, err
		}
		args = append(args, "-data", dataPath)
	}
	config := m.cfg.Text()
	if config != "" {
		cfgPath := filepath.Join(dir, "cfg.yml")
		err = os.WriteFile(cfgPath, []byte(config), 0o600)
		if err != nil {
			return nil, err
		}
		args = append(args, "-cfg", cfgPath)
	}
	srcPath := filepath.Join(dir, "src.cel")
	err = os.WriteFile(srcPath, []byte(src), 0o600)
	if err != nil {
		return nil, err
	}
	args = append(args, srcPath)
	cmd = execabs.Command("mito", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	ctxStdout, cancelStdout := context.WithCancel(context.Background())
	ctxStderr, cancelStderr := context.WithCancel(context.Background())
	go func() {
		defer cancelStdout()
		var buf [4096]byte
		for {
			n, err := stdout.Read(buf[:])
			if n != 0 {
				m.results <- text{data: string(buf[:n]), tag: "output"}
			}
			var pe *fs.PathError
			switch {
			case err == nil:
			case err == io.EOF, errors.As(err, &pe) && pe.Err == fs.ErrClosed:
				return
			default:
				log.Println(err)
				return
			}
		}
	}()
	go func() {
		defer cancelStderr()
		var buf [4096]byte
		for {
			n, err := stderr.Read(buf[:])
			if n != 0 {
				m.results <- text{data: string(buf[:n]), tag: "error"}
			}
			var pe *fs.PathError
			switch {
			case err == nil:
			case err == io.EOF, errors.As(err, &pe) && pe.Err == fs.ErrClosed:
				return
			default:
				log.Println(err)
				return
			}
		}
	}()
	err = cmd.Start()
	if err != nil {
		return nil, err
	}
	go func() {
		<-ctxStdout.Done()
		<-ctxStderr.Done()
		cmd.Wait()
		m.ps.Store(nil)
		if !keep {
			os.RemoveAll(dir)
		}
	}()
	return cmd.Process, nil
}

func (m *miko) celfmt() (string, error) {
	text := m.src.Text()
	if strings.TrimSpace(text) == "" {
		return "", nil
	}
	cmd := execabs.Command("celfmt")
	cmd.Stdin = strings.NewReader(text)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		if stderr.Len() == 0 {
			return "", err
		}
		return "", fmt.Errorf("celfmt: %s", &stderr)
	}
	return stdout.String(), nil
}

func (m *miko) jsonfmt() (string, error) {
	text := m.data.Text()
	if strings.TrimSpace(text) == "" {
		return "", nil
	}
	var buf bytes.Buffer
	err := json.Indent(&buf, []byte(text), "", "\t")
	if err != nil {
		return "", err
	}
	return buf.String(), nil
}
