FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
    && apt-get install -y --no-install-recommends build-essential ca-certificates \
    && rm -rf /var/lib/apt/lists/*

RUN printf '%s\n' \
    '#include <stdio.h>' \
    'int main(void) {' \
    '    puts("hello from the Linux multi-arch image");' \
    '    return 0;' \
    '}' > /tmp/macker-test.c \
    && cc -O2 -Wall -Wextra -o /usr/local/bin/macker-test-hello /tmp/macker-test.c \
    && rm /tmp/macker-test.c

CMD ["/usr/local/bin/macker-test-hello"]
