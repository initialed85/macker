FROM nginx:latest

ARG NGINX_PORT=8080

# Keep the Linux image on the same port as the native Darwin Macker image so
# the multi-platform tag has the same basic network contract.
RUN sed -i -E \
        -e "s/listen[[:space:]]+\[::\]:80([;[:space:]])/listen [::]:${NGINX_PORT}\\1/g" \
        -e "s/listen[[:space:]]+80([;[:space:]])/listen ${NGINX_PORT}\\1/g" \
        /etc/nginx/conf.d/default.conf \
    && grep -q "listen ${NGINX_PORT}" /etc/nginx/conf.d/default.conf

EXPOSE ${NGINX_PORT}
