package telsh

import (
	"bytes"
	telnet "github.com/p12s/fake-device"
	oi "github.com/reiver/go-oi"
	"io"
	"strings"
	"sync"
	"time"
)

const (
	defaultExitCommandName = "exit"
	defaultPrompt          = "§ "
	defaultWelcomeMessage  = "\r\nWelcome!\r\n"
	defaultExitMessage     = "\r\nGoodbye!\r\n"

	defaultAskLogin             = "User: "
	defaultLogin                = "admin"
	defaultAskPassword          = "Password: "
	defaultPassword             = "admin"
	defaultFirmwareVersion      = "This is default firmware version:\nv0.0.1 2023.05.30"
	defaultAskVersion           = "get"
	defaultUpdateVersion        = "set"
	defaultMaxRetry             = 3
	defaultLongInternalTimeout  = 60 * time.Second
	defaultShortInternalTimeout = 3 * time.Second

	stateLogin          = 10
	statePassword       = 20
	stateCommandProcess = 30

	// troubles management commands
	startImitateBrakeConnect         = "startImitateBrakeConnect"
	startImitateInternalLongResponse = "startImitateInternalLongResponse"
	startImitateLongCommandResponses = "startImitateLongCommandResponses"
	stopImitateLongCommandResponses  = "stopImitateLongCommandResponses"
)

// для нескольких одновременных соединений
// поймать коннект, взять уникальный ид, проверить что его еще нет
// если есть - отметить флаг повторного запроса
// нет - положить в контекст и отправить дальше
type auth struct {
	login    string
	password string
	retry    int
	state    int
}

type ShellHandler struct {
	muxtex       sync.RWMutex
	producers    map[string]Producer
	elseProducer Producer

	// settings
	ExitCommandName    string
	Prompt             string
	WelcomeMessage     string
	AskLoginMessage    string
	AskPasswordMessage string
	ExitMessage        string

	// troubles imitation
	ImitateInternalLongResponse bool
	ImitateLongCommandResponses bool

	// TODO make session store, device must support multiple concurrent connections (1 - wait login, 2 - command #453)
	login    string
	password string
	retry    int
}

func NewShellHandler() *ShellHandler {
	producers := map[string]Producer{}

	h := ShellHandler{
		producers: producers,

		Prompt:             defaultPrompt,
		ExitCommandName:    defaultExitCommandName,
		WelcomeMessage:     defaultWelcomeMessage,
		AskLoginMessage:    defaultAskLogin,
		AskPasswordMessage: defaultAskPassword,
		ExitMessage:        defaultExitMessage,
		//connectMap:      make(map[int]auth),
	}

	return &h
}

func (h *ShellHandler) Register(name string, producer Producer) error {

	h.muxtex.Lock()
	h.producers[name] = producer
	h.muxtex.Unlock()

	return nil
}

func (h *ShellHandler) MustRegister(name string, producer Producer) *ShellHandler {
	if err := h.Register(name, producer); nil != err {
		panic(err)
	}

	return h
}

func (h *ShellHandler) RegisterHandlerFunc(name string, handlerFunc HandlerFunc) error {

	produce := func(ctx telnet.Context, name string, args ...string) Handler {
		return PromoteHandlerFunc(handlerFunc, args...)
	}

	producer := ProducerFunc(produce)

	return h.Register(name, producer)
}

func (h *ShellHandler) MustRegisterHandlerFunc(name string, handlerFunc HandlerFunc) *ShellHandler {
	if err := h.RegisterHandlerFunc(name, handlerFunc); nil != err {
		panic(err)
	}

	return h
}

func (h *ShellHandler) RegisterElse(producer Producer) error {

	h.muxtex.Lock()
	h.elseProducer = producer
	h.muxtex.Unlock()

	return nil
}

func (h *ShellHandler) MustRegisterElse(producer Producer) *ShellHandler {
	if err := h.RegisterElse(producer); nil != err {
		panic(err)
	}

	return h
}

func (h *ShellHandler) ServeTELNET(ctx telnet.Context, writer telnet.Writer, reader telnet.Reader) {

	logger := ctx.Logger()
	if nil == logger {
		logger = internalDiscardLogger{}
	}

	colonSpaceCommandNotFoundEL := []byte(": command not found :(\r\n")

	var prompt bytes.Buffer
	prompt.WriteString(h.Prompt)
	promptBytes := prompt.Bytes()

	exitCommandName := h.ExitCommandName
	welcomeMessage := h.WelcomeMessage
	exitMessage := h.ExitMessage
	firmwareVersion := defaultFirmwareVersion

	if _, err := oi.LongWriteString(writer, welcomeMessage); nil != err {
		logger.Errorf("Problem long writing welcome message: %v", err)
		return
	}
	logger.Debugf("Wrote welcome message: %q.", welcomeMessage)

	// ask login
	state := stateLogin
	if _, err := oi.LongWrite(writer, []byte(h.AskLoginMessage)); nil != err {
		logger.Errorf("Ask login writing prompt: %v", err)
		return
	}
	logger.Debugf("Wrote ask login: %q.", []byte(h.AskLoginMessage))

	var buffer [1]byte // Seems like the length of the buffer needs to be small, otherwise will have to wait for buffer to fill up.
	p := buffer[:]

	var line bytes.Buffer

	for {
		// Read 1 byte.
		n, err := reader.Read(p)
		if n <= 0 && nil == err {
			continue
		} else if n <= 0 && nil != err {
			break
		}

		line.WriteByte(p[0])
		//logger.Tracef("Received: %q (%d).", p[0], p[0])

		if '\n' == p[0] {
			lineString := line.String()

			switch state {
			case stateLogin: // проверить что прилетает строка логина
				h.login = strings.TrimSpace(lineString) // положить в логин, если надо
				line.Reset()
				state = statePassword
				// ask password
				if _, err := oi.LongWrite(writer, []byte(h.AskPasswordMessage)); nil != err {
					logger.Errorf("Ask password writing prompt: %v", err)
					return
				}
				logger.Debugf("Wrote ask password: %q.", []byte(h.AskPasswordMessage))
				continue
			case statePassword:
				h.password = strings.TrimSpace(lineString)
				line.Reset()
				if h.login == defaultLogin && h.password == defaultPassword {
					state = stateCommandProcess
					if _, err := oi.LongWrite(writer, promptBytes); nil != err {
						logger.Errorf("Problem long writing prompt: %v", err)
						return
					}
					logger.Debugf("Wrote prompt: %q.", promptBytes)
				} else {
					// возможно понадобится читать кол-во попыток ввода логин/пасс
					state = stateLogin
					if _, err := oi.LongWrite(writer, []byte(h.AskLoginMessage)); nil != err {
						logger.Errorf("Ask login writing prompt: %v", err)
						return
					}
					logger.Debugf("Wrote ask login: %q.", []byte(h.AskLoginMessage))
				}
				h.login = ""
				h.password = ""
				h.retry += 1
				continue
			}

			if "\r\n" == lineString {
				line.Reset()
				if _, err := oi.LongWrite(writer, promptBytes); nil != err {
					return
				}
				continue
			}

			//@TODO: support piping.
			fields := strings.Fields(lineString)
			logger.Debugf("Have %d tokens.", len(fields))
			logger.Tracef("Tokens: %v", fields)
			if len(fields) <= 0 {
				line.Reset()
				if _, err := oi.LongWrite(writer, promptBytes); nil != err {
					return
				}
				continue
			}

			field0 := fields[0]

			switch field0 {
			case exitCommandName, startImitateBrakeConnect:
				oi.LongWriteString(writer, exitMessage)
				return

			case defaultAskVersion:
				if _, err := oi.LongWrite(writer, []byte(firmwareVersion+"\n")); nil != err {
					return
				}
				line.Reset()
				if _, err := oi.LongWrite(writer, promptBytes); nil != err {
					return
				}
				continue

			case defaultUpdateVersion:
				if len(fields) <= 1 {
					fields = append(fields, " ")
				}
				firmwareVersion = strings.Join(fields[1:], " ")
				line.Reset()
				if _, err := oi.LongWrite(writer, promptBytes); nil != err {
					return
				}
				continue

			case startImitateInternalLongResponse:
				time.Sleep(defaultLongInternalTimeout)

			case startImitateLongCommandResponses:
				h.ImitateLongCommandResponses = true
			case stopImitateLongCommandResponses:
				h.ImitateLongCommandResponses = false
			}

			if h.ImitateLongCommandResponses {
				time.Sleep(defaultShortInternalTimeout)
			}

			var producer Producer

			h.muxtex.RLock()
			var ok bool
			producer, ok = h.producers[field0]
			h.muxtex.RUnlock()

			if !ok {
				h.muxtex.RLock()
				producer = h.elseProducer
				h.muxtex.RUnlock()
			}

			if nil == producer {
				//@TODO: Don't convert that to []byte! think this creates "garbage" (for collector).
				oi.LongWrite(writer, []byte(field0))
				oi.LongWrite(writer, colonSpaceCommandNotFoundEL)
				line.Reset()
				if _, err := oi.LongWrite(writer, promptBytes); nil != err {
					return
				}
				continue
			}

			handler := producer.Produce(ctx, field0, fields[1:]...)
			if nil == handler {
				oi.LongWrite(writer, []byte(field0))
				//@TODO: Need to use a different error message.
				oi.LongWrite(writer, colonSpaceCommandNotFoundEL)
				line.Reset()
				oi.LongWrite(writer, promptBytes)
				continue
			}

			//@TODO: Wire up the stdin, stdout, stderr of the handler.

			if stdoutPipe, err := handler.StdoutPipe(); nil != err {
				//@TODO:
			} else if nil == stdoutPipe {
				//@TODO:
			} else {
				connect(ctx, writer, stdoutPipe)
			}

			if stderrPipe, err := handler.StderrPipe(); nil != err {
				//@TODO:
			} else if nil == stderrPipe {
				//@TODO:
			} else {
				connect(ctx, writer, stderrPipe)
			}

			if err := handler.Run(); nil != err {
				//@TODO:
			}
			line.Reset()
			if _, err := oi.LongWrite(writer, promptBytes); nil != err {
				return
			}
		}

		//@TODO: Are there any special errors we should be dealing with separately?
		if nil != err {
			break
		}
	}

	oi.LongWriteString(writer, exitMessage)
	return
}

func connect(ctx telnet.Context, writer io.Writer, reader io.Reader) {

	logger := ctx.Logger()

	go func(logger telnet.Logger) {

		var buffer [1]byte // Seems like the length of the buffer needs to be small, otherwise will have to wait for buffer to fill up.
		p := buffer[:]

		for {
			// Read 1 byte.
			n, err := reader.Read(p)
			if n <= 0 && nil == err {
				continue
			} else if n <= 0 && nil != err {
				break
			}

			//logger.Tracef("Sending: %q.", p)
			//@TODO: Should we be checking for errors?
			oi.LongWrite(writer, p)
			//logger.Tracef("Sent: %q.", p)
		}
	}(logger)
}
