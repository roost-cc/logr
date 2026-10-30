BIN     := bin
SERVER  := $(BIN)/logr-server
IN      := $(BIN)/logr-in
VIEW    := $(BIN)/logr-view
SOURCES := $(shell find cmd internal -name '*.go')

.PHONY: all clean test

all: $(BIN) $(SERVER) $(IN) $(VIEW)

$(BIN):
	@mkdir -p $(BIN)

$(SERVER): $(SOURCES)
	go build -o $@ ./cmd/logr-server

$(IN): $(SOURCES)
	go build -o $@ ./cmd/logr-in

$(VIEW): $(SOURCES)
	go build -o $@ ./cmd/logr-view

test:
	go test ./...

clean:
	rm -rf $(BIN)
