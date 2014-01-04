// Package girc is a library for acting as an IRC client.
package girc

import (
	"bufio"
	"errors"
	"fmt"
	"log"
	"net"
	"os/user"
	"strings"
)

// struct Command describes an IRC command. Any IRC command
// consists of three parts: An optional source hostname,
// the type of the command (for example PING, or PRIVMSG)
// followed by a number of arguments. Only the final argument
// may have spaces in it. These are represented with Source,
// Type, and Args respectively.
type Command struct {
	Source string
	Type   string
	Args   []string
}

// struct Connection describes an IRC connection. Location
// is the URI for the server, and Nick is the nick we wish to
// use. Connection also includes a Finished channel that will
// be closed when the Connection itself is closed.
type Connection struct {
	Location  string
	Nick      string
	conn      net.Conn
	listeners []chan *Command
	Finished  chan bool
}

// New creates a new Connection given the uri of the server
// and the nick we wish to use. The returned connection is
// not actually connected yet. This is in order to give the
// programmer time to actually add listeners if they wish
// to listen in on message interchanges during the connection
// process.
func New(uri string, nick string) *Connection {
	var connection Connection

	connection.Location = uri
	connection.Nick = nick

	return &connection
}

// Raw turns a given Command into its Raw form. See RFC 1459
// section 2.3 <http://tools.ietf.org/html/rfc1459.html#section-2.3>
// for details on how this is accomplished.
func (command *Command) Raw() (string, error) {
	out := []string{}
	if command.Source != "" {
		out = append(out, command.Source)
	}
	out = append(out, command.Type)
	for _, arg := range command.Args[0 : len(command.Args)-1] {
		if strings.Contains(arg, " ") {
			return "", errors.New("nonfinal argument contains space")
		}
		out = append(out, arg)
	}

	if strings.Contains(command.Args[len(command.Args)-1], " ") {
		out = append(out, fmt.Sprint(":", command.Args[len(command.Args)-1]))
	} else {
		out = append(out, command.Args[len(command.Args)-1])
	}

	return fmt.Sprintf("%s\r\n", strings.Join(out, " ")), nil
}

// SendCommand sends a given command to the server on the given connection
func (connection *Connection) SendCommand(command *Command) error {
	raw_form, err := command.Raw()
	if err != nil {
		return err
	}

	fmt.Fprint(connection.conn, raw_form)

	return nil
}

// AddListener adds a new channel as a listener to the given connection. Any
// incoming messages will be converted to their Command form and
// written to the given channel. This channel should probably be
// buffered for the best performance. It will not cause the connection
// routine to hang if this is not the case as it will create goroutines
// on the fly to handle this, but if the channel is appropriately buffered
// this will not be necessary. After the connection is closed this channel
// will be closed.
func (connection *Connection) AddListener(channel chan *Command) {
	connection.listeners = append(connection.listeners, channel)
}

// Send sends a command. This is basically short for creating a command and
// sending it using SendCommand. This function takes the command type (for
// example PRIVMSG, PING, etc) and a variadic list of arguments for that command.
func (connection *Connection) Send(cmdtype string, args ...string) error {
	var command Command

	command.Type = cmdtype
	command.Args = args

	err := connection.SendCommand(&command)

	return err
}

// Close closes the connection. It does this by simply closing
// the actual TCP connection. The connection thread will notice
// this and appropriately call close() on all the listening
// channels it has at the time.
func (connection *Connection) Close() error {
	/*
	 * close the connection to the server.
	 * A good IRC client should probably
	 * issue a QUIT command before doing this
	 */
	err := connection.conn.Close()
	/*
	 * After this happens the consuming thread should
	 * notice the connection is closed and close all
	 * the receiving channels, causing their threads to
	 * die
	 */

	return err
}

// Connect actual causes the given connection to open
// a TCP connection. In addition to this, it spins off
// two goroutines. One listens for and handles incoming
// messages from the server. The other simply responds to
// PINGs automatically. After this it registers the requested
// NICK with the server and issues a USER command to complete
// the connection.
func (connection *Connection) Connect() error {
	conn, err := net.Dial("tcp", connection.Location)
	connection.conn = conn

	if err != nil {
		return err
	}

	// Create a new goroutine to handle incoming
	// messages and relay them to all our listeners
	go func() {
		for {
			line, err := bufio.NewReader(conn).ReadString('\n')
			if err != nil {
				for _, channel := range connection.listeners {
					close(channel)
				}
				log.Fatal(err)
			}
			command, err := rawToCommand(line)
			if err != nil {
				log.Print(err)
				continue
			}
			for _, channel := range connection.listeners {
				// try to write to the channel. If the buffer is
				// full just make a goroutine to write to it at a
				// later point
				select {
				case channel <- command:
				default:
					go func() {
						channel <- command
					}()
				}
			}
		}
	}()

	// create a routine to send PONGs back when we get them
	go func() {
		// a buffer of 10 should be enough for anyone, right!(?)
		command_chan := make(chan *Command, 10)
		connection.AddListener(command_chan)

		for {
			command, ok := <-command_chan

			if !ok {
				break
			}

			if command.Type == "PING" {
				if len(command.Args) < 1 {
					log.Printf("Malformed PING command: %v\n")
				} else {
					connection.Send("PONG", command.Args[0])
				}
			}
		}
	}()

	err = connection.Send("NICK", connection.Nick)
	if err != nil {
		return err
	}
	/*
	 * query the local system for a username. This isn't *really* necessary,
	 * but it really isn't that big of a deal to do it away
	 */
	user, err := user.Current()
	if err != nil {
		log.Print(err)
		user.Username = "unknown"
	}

	err = connection.Send("USER", user.Username, "0", "*", "An IRC bot built with girc")

	if err != nil {
		return err
	}

	return nil
}

func rawToCommand(raw string) (*Command, error) {
	var command Command

	split_ver := strings.Split(raw, " ")
	/* first as a sanity check make sure that our array has at least
	   two entries, any less is not a valid command */
	if len(split_ver) < 2 {
		return &command, errors.New("invalid command (less than two entries in command)")
	}
	args_start := 2
	if strings.HasPrefix(split_ver[0], ":") {
		command.Source = strings.TrimPrefix(split_ver[0], ":")
		command.Type = split_ver[1]
	} else {
		command.Type = split_ver[0]
		args_start = 1
	}

	/* iterate over every element after the first two */
	multi_word_index := -1
	for index, arg := range split_ver[args_start:] {
		if strings.HasPrefix(arg, ":") {
			multi_word_index = index
			break
		}

		command.Args = append(command.Args, arg)
	}

	if multi_word_index != -1 {
		words := []string{}
		words = append(words, split_ver[args_start:][multi_word_index][1:len(split_ver[args_start:][multi_word_index])])
		words = append(words, split_ver[args_start:][multi_word_index+1:]...)
		command.Args = append(command.Args, strings.Join(words, " "))
	}

	command.Args[len(command.Args)-1] = strings.TrimSuffix(command.Args[len(command.Args)-1], "\r\n")

	return &command, nil
}
