package main

import (
	"bufio"
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
	. "modernc.org/tk9.0/extensions/autoscroll"
)

func main() {
	txt := flag.String("txtar", "", "txtar file containing src.cel, data.json and cfg.yaml (incompatible with any other argument)")
	srcPath := flag.String("src", "", "path to a CEL program")
	dataPath := flag.String("data", "", "path to a JSON object holding input (exposed as the label state)")
	cfgPath := flag.String("cfg", "", "path to a YAML file holding run control configuration (see pkg.go.dev/github.com/elastic/mito/cmd/mito)")
	font := flag.String("font", "Courier", "font family")
	size := flag.Uint("face_size", 10, "font face size")
	tw := flag.Uint("tw", 4, "width of tab stops measured in spaces")
	poll := flag.Duration("fr", 10*time.Millisecond, "refresh poll rate")
	flag.Parse()
	if *txt != "" && (*dataPath != "" || *cfgPath != "" || *srcPath != "") || *tw == 0 {
		flag.Usage()
		os.Exit(2)
	}
	m := newMiko(*font, int(*size), int(*tw), *poll)
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
	ps          atomic.Pointer[os.Process]
	results     chan text
	src         *TextWidget
	data        *TextWidget
	cfg         *TextWidget
	display     *TextWidget
	insecure    bool
	logRequests bool
	dumpCrash   bool
}

type text struct {
	data string
	tag  string
}

func newMiko(font string, size, tw int, poll time.Duration) *miko {
	App.WmTitle("miko")
	// Allow the main window to be resized.
	App.SetResizable(true, true)
	// Only render scroll bars when needed.
	InitializeExtension("autoscroll")

	m := &miko{results: make(chan text)}

	// Use a TPanedwindow with a horizontal orientation for the main layout.
	// This will create two panes (left and right) separated by a movable sash.
	paned := App.TPanedwindow(Orient("horizontal"))
	Grid(paned, Row(0), Column(0), Sticky("news"))

	// Configure the main window's grid so that the paned window expands
	// to fill the available space when the window is resized.
	GridRowConfigure(App, 0, Weight(1))
	GridColumnConfigure(App, 0, Weight(1))

	// Create frames for the left and right panes.
	leftPane := App.Frame()
	rightPane := App.Frame()

	// Add the frames to the paned window. The 'Weight' option determines
	// the initial size ratio between the panes.
	paned.Add(leftPane.Window, Weight(1))
	paned.Add(rightPane.Window, Weight(2))

	// --- Configure the Left Pane ---
	// This pane will contain the control buttons and the three input text widgets.
	// Configure its grid so that the content can expand horizontally.
	GridColumnConfigure(leftPane, 0, Weight(1))

	buttons := leftPane.Frame()

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

	insecure := buttons.Window.Checkbutton(
		Txt("Insecure HTTPS"),
		Variable(&m.insecure),
		Command(func() { m.insecure = !m.insecure }),
	)

	logRequests := buttons.Window.Checkbutton(
		Txt("Log Requests"),
		Variable(&m.insecure),
		Command(func() { m.logRequests = !m.logRequests }),
	)

	dumpCrash := buttons.Window.Checkbutton(
		Txt("Dump Crashes"),
		Variable(&m.insecure),
		Command(func() { m.dumpCrash = !m.dumpCrash }),
	)

	buttonLayout := [][]Widget{
		{run, cancel, format, snarf, clear},
		{insecure, logRequests, dumpCrash},
	}
	for i, r := range buttonLayout {
		for j, b := range r {
			Grid(b, Row(i), Column(j), Sticky("news"))
			GridColumnConfigure(buttons.Window, j, Weight(1))
		}
	}
	// Place the buttons frame in the first row of the left pane.
	// It should expand horizontally ("ew") but not vertically.
	Grid(buttons, Row(0), Column(0), Sticky("ew"))

	face := NewFont(Family(font), Size(size))
	tabWidth := face.Measure(App, strings.Repeat(" ", tw))

	// Create and place the three input text widgets in the left pane.
	for i, input := range []struct {
		name string
		text **TextWidget
	}{
		{name: "src (CEL)", text: &m.src},
		{name: "data (JSON)", text: &m.data},
		{name: "cfg (YAML)", text: &m.cfg},
	} {
		// Each text widget gets its own frame.
		frame := leftPane.Frame()
		textWidget(input.text, frame, input.name, face, tabWidth, true)
		Grid(frame, Row(i+1), Column(0), Sticky("news"))
		// Configure the row in the left pane to expand vertically.
		GridRowConfigure(leftPane, i+1, Weight(1))
	}

	// --- Configure the Right Pane ---
	// This pane contains the single output display widget.
	// Configure its grid to allow the content to expand in both directions.
	GridRowConfigure(rightPane, 0, Weight(1))
	GridColumnConfigure(rightPane, 0, Weight(1))
	displayFrame := rightPane.Frame()
	textWidget(&m.display, displayFrame, "", face, tabWidth, false)
	Grid(displayFrame, Row(0), Column(0), Sticky("news"))

	m.display.Configure(State("disabled"))
	m.display.TagConfigure("output", Foreground("black"))
	m.display.TagConfigure("error", Foreground("red"))

	Focus(m.src)

	NewTicker(poll, func() {
		select {
		case text := <-m.results:
			m.display.Configure(State("normal"))
			m.display.Insert("end", text.data+"\n", text.tag)
			m.display.See(END)
			m.display.Configure(State("disabled"))
		default:
		}
	})

	return m
}

func textWidget(dst **TextWidget, frame *FrameWidget, title string, face *FontFace, tabWidth int, undo bool) {
	w := frame.Window
	// Configure the grid within the widget's frame to allow the text area to expand.
	GridRowConfigure(w, 1, Weight(1))
	GridColumnConfigure(w, 0, Weight(1))

	scrollX := Autoscroll(w.TScrollbar(Command(func(e *Event) { e.Xview(*dst) }), Orient("horizontal")).Window)
	scrollY := Autoscroll(w.TScrollbar(Command(func(e *Event) { e.Yview(*dst) }), Orient("vertical")).Window)
	*dst = w.Text(
		Font(face),
		Tabs(tabWidth),
		Undo(undo),
		Wrap("none"),
		Background(White),
		Padx("1m"), Pady("1m"),
		Blockcursor(false),
		Insertunfocussed("hollow"),
		Xscrollcommand(func(e *Event) { e.ScrollSet(scrollX) }),
		Yscrollcommand(func(e *Event) { e.ScrollSet(scrollY) }),
	)
	if title != "" {
		Grid(w.Label(Anchor("w"), Txt(title)), Row(0), Column(0), Sticky("w"))
	}
	// The text widget expands in all directions ("news").
	Grid(*dst, Row(1), Column(0), Sticky("news"))
	// The scrollbars only expand in their respective directions.
	Grid(scrollY, Row(1), Column(1), Sticky("ns"))
	Grid(scrollX, Row(2), Column(0), Sticky("ew"))
}

func (m *miko) printError(err error) {
	if err == nil {
		return
	}
	m.display.Configure(State("normal"))
	m.display.Insert("end", err.Error()+"\n", "error")
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
	if m.insecure {
		args = append(args, "-insecure")
	}
	if m.logRequests {
		args = append(args, "-log_requests")
	}
	if m.dumpCrash {
		args = append(args, "-dump", "error")
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
		dec := json.NewDecoder(stdout)
		for {
			var v any
			err := dec.Decode(&v)
			var pe *fs.PathError
			switch {
			case err == nil:
				b, err := json.MarshalIndent(v, "", "\t")
				if err != nil {
					log.Println(err)
					return
				}
				m.results <- text{data: string(b), tag: "output"}
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
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			m.results <- text{data: sc.Text(), tag: "error"}
		}
		err := sc.Err()
		var pe *fs.PathError
		switch {
		case err == nil:
		case err == io.EOF, errors.As(err, &pe) && pe.Err == fs.ErrClosed:
			return
		default:
			log.Println(err)
			return
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
	return strings.TrimSpace(stdout.String()), nil
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
	return strings.TrimSpace(buf.String()), nil
}
