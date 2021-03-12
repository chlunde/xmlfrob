/*
xmlfrob implements a program for making minor modifications to XML
files.

Goals:

 - more robust for XML files than sed
 - simpler than xsltproc
 - keep style, indentation and comments of original input file

Example:

  <server>
      <connector port="8080"/>
  </server>

  xmlfrob --inplace --input foo.xml /server/connector@port=8181
*/

package main

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"syscall"
)

// a modification contains an element path, attribute name and the new
// value for the attribute
type modification struct {
	path      string
	attribute string
	value     string
}

// parseModifications parses modification strings to structs:
//
//     /foo/bar@attr=val
//
// parses to:
//     path:      /foo/bar
//     attribute: attr
//     value:     val
func parseModifications(modStrings []string) ([]modification, error) {
	modifications := make([]modification, len(modStrings))
	for i, mod := range modStrings {
		// input: /foo/bar@attr=val

		// /foo/bar@attr, val
		pathAttrValue := strings.SplitN(mod, "=", 2)

		// /foo/bar, attr
		pathAttr := strings.SplitN(pathAttrValue[0], "@", 2)

		if len(pathAttrValue) != 2 || len(pathAttr) != 2 {
			return nil, fmt.Errorf(`Invalid mod "%s": expected syntax /xml/path@attr=newValue`, mod)
		}

		modifications[i] = modification{
			path:      pathAttr[0],
			attribute: pathAttr[1],
			value:     pathAttrValue[1],
		}
	}

	return modifications, nil
}

// frobnicate applies modifications to the XML input stream and
// returns the modified XML
func frobnicate(in io.Reader, modifications []modification) (*bytes.Buffer, error) {
	decoder := xml.NewDecoder(bufio.NewReader(in))

	var outbytes bytes.Buffer
	out := xml.NewEncoder(&outbytes)
	var previousWasStart bool
	var path bytes.Buffer
	skip := false
	pluginNo := 0
	for {
		tok, err := decoder.RawToken()
		if err != nil {
			if err == io.EOF {
				break
			}
			log.Fatalf("Unexpected error while parsing XML file: %v", err)
		}
		switch tok := tok.(type) {
		case xml.StartElement:
			path.WriteByte('/')
			path.WriteString(tok.Name.Local)

			if path.String() == "/project/profiles/profile/build/plugins/plugin" {
				skip = pluginNo == 0 || pluginNo == 1
				pluginNo++
			}

			for _, pat := range modifications {
				if path.String() == pat.path {
					for i, attr := range tok.Attr {
						if attr.Name.Local == pat.attribute {
							tok.Attr[i].Value = pat.value
						}
					}
				}
			}

			previousWasStart = true
			if !skip {
				if err := out.EncodeToken(tok); err != nil {
					return nil, err
				}
			}

		case xml.EndElement:
			cur := path.String()
			path.Truncate(bytes.LastIndexByte(path.Bytes(), '/'))
			if !skip {
				if previousWasStart {
					// hack: Replace <foo></foo> with self-closing tags <foo/>
					// https://groups.google.com/forum/#!topic/golang-nuts/guG6iOCRu08
					if err := out.Flush(); err != nil {
						return nil, err
					}

					if outbytes.Bytes()[outbytes.Len()-1] != '>' {
						panic("expected > token as last byte in output")
					}
					pos := outbytes.Len() - 1

					// Encode end element so the encoder is not confused..
					if err := out.EncodeToken(tok); err != nil {
						return nil, err
					}

					if err := out.Flush(); err != nil {
						return nil, err
					}

					// Back track to before end element and final >
					outbytes.Truncate(pos)
					outbytes.WriteString("/>")
				} else {
					if err := out.EncodeToken(tok); err != nil {
						return nil, err
					}
				}
				previousWasStart = false
			}

			if cur == "/project/profiles/profile/build/plugins/plugin" {
				skip = false
			}

		default:
			previousWasStart = false
			if !skip {
				if err := out.EncodeToken(tok); err != nil {
					return nil, err
				}
			}
		}
	}

	return &outbytes, out.Flush()
}

func usage(message string) {
	fmt.Fprintf(os.Stderr, "Usage: %s [OPTIONS...] <PATTERNS...>\n\n", os.Args[0])
	fmt.Fprintf(os.Stderr, "Pattern syntax: /xml/patt@attr=val\n\n")
	if message != "" {
		fmt.Fprintf(os.Stderr, "%v\n", message)
	} else {
		flag.PrintDefaults()
	}
	os.Exit(1)
}

func main() {
	var (
		input   string
		inplace bool
	)

	flag.Usage = func() { usage("") }
	flag.StringVar(&input, "input", "-", "input XML file (default to stdin)")
	flag.BoolVar(&inplace, "inplace", false, "modify in place (save back to same file as input)")

	flag.Parse()

	//	if flag.NArg() == 0 {
	//	usage("At least one modification pattern required") // exits
	//}

	if inplace && input == "-" {
		fmt.Fprintf(os.Stderr, "Invalid arguments: cannot combine --inplace and --input - (stdin)\n")
		os.Exit(1)
	}

	modifications, err := parseModifications(flag.Args())
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	var in io.Reader
	if input == "-" {
		in = os.Stdin
	} else {
		var f *os.File
		f, err = os.Open(input)
		if err != nil {
			log.Fatal(err)
		}
		in = f
		defer func() {
			logInformationalError(f.Close())
		}()
	}

	outbuf, err := frobnicate(in, modifications)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	if inplace {
		err = writeInplace(input, outbuf)
	} else {
		_, err = io.Copy(os.Stdout, outbuf)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "could not write: %v\n", err)
		os.Exit(1)
	}
}

// writeInplace attempts to write replace the original file with new
// contents atomically, by writing to a temporary file and overwriting
// the original file using rename
func writeInplace(filename string, contents io.Reader) error {
	tempname := filename + ".tmp"
	output, err := os.Create(tempname)
	if err != nil {
		return err
	}

	_, err = io.Copy(output, contents)
	if err == nil {
		err = output.Sync()
	}

	if st, err := os.Stat(filename); err == nil {
		logInformationalError(output.Chmod(st.Mode()))

		if os.Getuid() == 0 {
			if ust, ok := st.Sys().(*syscall.Stat_t); ok {
				logInformationalError(output.Chown(int(ust.Uid), int(ust.Gid)))
			}
		}
	} else {
		logInformationalError(err)
	}

	logInformationalError(output.Close())

	if err != nil {
		logInformationalError(os.Remove(tempname))
		return fmt.Errorf("error while writing output: %v", err)
	}

	err = os.Rename(tempname, filename)
	if err != nil {
		logInformationalError(os.Remove(tempname))
		return fmt.Errorf("error while renaming temporary file to destination file: %v", err)
	}

	return nil
}

// Some errors, like failing to unlink the temporary file when
// cleaning up after a failure, can't be handled, but we should log
// them.  This function logs if error is non-nil
func logInformationalError(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}
