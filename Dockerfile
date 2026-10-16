FROM busybox:1.37.0-musl

ARG GIT_SHA=unknown

COPY index.html /www/index.html

RUN mkdir -p /www/sha \
 && printf 'git sha: %s\n' "$GIT_SHA" > /www/sha/index.html

USER 65534
EXPOSE 8080

CMD ["httpd", "-f", "-v", "-p", "8080", "-h", "/www"]
