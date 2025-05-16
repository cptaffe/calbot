FROM golang:1.24.2-alpine3.21

RUN apk add --no-cache \
    git \
    openssh \
    && mkdir $HOME/.ssh \
    && chmod 600 $HOME/.ssh \
    && printf 'Host github.com\n\tStrictHostKeyChecking no\n' >$HOME/.ssh/config \
    && printf '[url "ssh://git@github.com/"]\n\tinsteadOf = https://github.com/\n' >$HOME/.gitconfig

WORKDIR /usr/src/calbot

# pre-copy/cache go.mod for pre-downloading dependencies and only redownloading them in subsequent builds if they change
ENV GOPRIVATE=github.com/cptaffe
COPY go.mod go.sum ./
RUN --mount=type=ssh go mod download && go mod verify

COPY main.go .
RUN go build -v -o /usr/local/bin/calbot .

COPY . .

CMD ["/usr/local/bin/calbot", "--templates=/usr/src/calbot/templates"]
