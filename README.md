# xmlfrob

`xmlfrob` implements a program for making minor modifications to XML
files.

Goals:

1. more robust for XML files than sed
1. simpler than xsltproc
1. keep style, indentation and comments of original input file

Example:

    <server>
      <connector port="8080"/>
    </server>


    xmlfrob --inplace --input foo.xml /server/connector@port=8181
