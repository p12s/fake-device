package telsh

import (
	"bytes"
	"fmt"
	telnet "github.com/p12s/fake-device"
	oi "github.com/reiver/go-oi"
	"io"
	"strings"
	"sync"
)

const (
	defaultExitCommandName = "exit"
	defaultPrompt          = "§ "
	defaultWelcomeMessage  = "\r\nWelcome!\r\n"
	defaultExitMessage     = "\r\nGoodbye!\r\n"

	defaultAskLogin    = "\nUser: "
	defaultLogin       = "login"
	defaultAskPassword = "\nPassword: "
	defaultPassword    = "pass"

	STATE_WAIT_LOGIN    = 10
	STATE_WAIT_PASSWORD = 20
	STATE_WAIT_COMMAND  = 30
)

type ShellHandler struct {
	muxtex       sync.RWMutex
	producers    map[string]Producer
	elseProducer Producer

	ExitCommandName string
	Prompt          string
	WelcomeMessage  string
	ExitMessage     string

	// пока как будто польз один на весь сервер
	login    string
	password string
}

func NewShellHandler() *ShellHandler {
	producers := map[string]Producer{}

	telnetHandler := ShellHandler{
		producers: producers,

		Prompt:          defaultPrompt,
		ExitCommandName: defaultExitCommandName,
		WelcomeMessage:  defaultWelcomeMessage,
		ExitMessage:     defaultExitMessage,
	}

	return &telnetHandler
}

func (telnetHandler *ShellHandler) Register(name string, producer Producer) error {

	telnetHandler.muxtex.Lock()
	telnetHandler.producers[name] = producer
	telnetHandler.muxtex.Unlock()

	return nil
}

func (telnetHandler *ShellHandler) MustRegister(name string, producer Producer) *ShellHandler {
	if err := telnetHandler.Register(name, producer); nil != err {
		panic(err)
	}

	return telnetHandler
}

func (telnetHandler *ShellHandler) RegisterHandlerFunc(name string, handlerFunc HandlerFunc) error {

	produce := func(ctx telnet.Context, name string, args ...string) Handler {
		return PromoteHandlerFunc(handlerFunc, args...)
	}

	producer := ProducerFunc(produce)

	return telnetHandler.Register(name, producer)
}

func (telnetHandler *ShellHandler) MustRegisterHandlerFunc(name string, handlerFunc HandlerFunc) *ShellHandler {
	if err := telnetHandler.RegisterHandlerFunc(name, handlerFunc); nil != err {
		panic(err)
	}

	return telnetHandler
}

func (telnetHandler *ShellHandler) RegisterElse(producer Producer) error {

	telnetHandler.muxtex.Lock()
	telnetHandler.elseProducer = producer
	telnetHandler.muxtex.Unlock()

	return nil
}

func (telnetHandler *ShellHandler) MustRegisterElse(producer Producer) *ShellHandler {
	if err := telnetHandler.RegisterElse(producer); nil != err {
		panic(err)
	}

	return telnetHandler
}

func (telnetHandler *ShellHandler) ServeTELNET(ctx telnet.Context, writer telnet.Writer, reader telnet.Reader) {

	logger := ctx.Logger()
	if nil == logger {
		logger = internalDiscardLogger{}
	}

	colonSpaceCommandNotFoundEL := []byte(": command not found :(\r\n")

	var prompt bytes.Buffer
	var exitCommandName string
	var welcomeMessage string
	var exitMessage string

	prompt.WriteString(telnetHandler.Prompt)

	promptBytes := prompt.Bytes()

	exitCommandName = telnetHandler.ExitCommandName
	welcomeMessage = telnetHandler.WelcomeMessage
	exitMessage = telnetHandler.ExitMessage

	if _, err := oi.LongWriteString(writer, welcomeMessage); nil != err {
		logger.Errorf("Problem long writing welcome message: %v", err)
		return
	}
	logger.Debugf("Wrote welcome message: %q.", welcomeMessage)

	// prompt - рано, сначала логин/пароль
	//if _, err := oi.LongWrite(writer, promptBytes); nil != err {
	//	logger.Errorf("Problem long writing prompt: %v", err)
	//	return
	//}
	//logger.Debugf("Wrote prompt: %q.", promptBytes)

	// ask login
	state := STATE_WAIT_LOGIN
	if telnetHandler.login == "" {
		if _, err := oi.LongWrite(writer, []byte(defaultAskLogin)); nil != err {
			logger.Errorf("Ask login writing prompt: %v", err)
			return
		}
		logger.Debugf("Wrote ask login: %q.", []byte(defaultAskLogin))
	}

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
			case STATE_WAIT_LOGIN: // проверить что прилетает строка логина
				telnetHandler.login = strings.TrimSpace(lineString) // положить в логин, если надо
				state = STATE_WAIT_PASSWORD
				fmt.Printf("login: `%s`\n", telnetHandler.login)
				// запросить пароль
				// ask password
				if telnetHandler.password == "" {
					if _, err := oi.LongWrite(writer, []byte(defaultAskPassword)); nil != err {
						logger.Errorf("Ask password writing prompt: %v", err)
						return
					}
					logger.Debugf("Wrote ask password: %q.", []byte(defaultAskPassword))
				}
				continue
			case STATE_WAIT_PASSWORD:
				telnetHandler.password = strings.TrimSpace(lineString)
				if telnetHandler.login == defaultLogin && telnetHandler.password == defaultPassword {
					fmt.Printf("success login\n")
					state = STATE_WAIT_COMMAND
				} else {
					fmt.Printf("login fail:\n")
					telnetHandler.login = ""
					telnetHandler.password = ""
					state = STATE_WAIT_LOGIN
				}
				continue
			case STATE_WAIT_COMMAND:
				fmt.Printf("do command:\n")
			}

			// положить в пароль
			// сравнить и либо  на 2 круг, либо в свободный режим

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

			if exitCommandName == field0 {
				oi.LongWriteString(writer, exitMessage)
				return
			}

			var producer Producer

			telnetHandler.muxtex.RLock()
			var ok bool
			producer, ok = telnetHandler.producers[field0]
			telnetHandler.muxtex.RUnlock()

			if !ok {
				telnetHandler.muxtex.RLock()
				producer = telnetHandler.elseProducer
				telnetHandler.muxtex.RUnlock()
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
