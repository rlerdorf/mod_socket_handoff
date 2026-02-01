/*
 * handoff.c - SCM_RIGHTS file descriptor receiving
 */
#include "handoff.h"
#include "json.h"
#include <string.h>
#include <unistd.h>
#include <errno.h>
#include <sys/socket.h>

void handoff_prepare_msghdr(struct msghdr *msg, struct iovec *iov,
                            char *data_buf, size_t data_buf_len,
                            char *ctrl_buf, size_t ctrl_buf_len) {
    memset(msg, 0, sizeof(*msg));
    memset(iov, 0, sizeof(*iov));

    iov->iov_base = data_buf;
    iov->iov_len = data_buf_len;

    msg->msg_iov = iov;
    msg->msg_iovlen = 1;
    msg->msg_control = ctrl_buf;
    msg->msg_controllen = ctrl_buf_len;
}

int handoff_extract_fd(struct msghdr *msg) {
    struct cmsghdr *cmsg = CMSG_FIRSTHDR(msg);
    if (!cmsg) {
        return -1;
    }

    if (cmsg->cmsg_level != SOL_SOCKET || cmsg->cmsg_type != SCM_RIGHTS) {
        return -1;
    }

    int fd;
    memcpy(&fd, CMSG_DATA(cmsg), sizeof(fd));
    return fd;
}

int handoff_recv_fd(int unix_fd, int *client_fd,
                    char *data, size_t *data_len) {
    struct msghdr msg = {0};
    struct iovec iov = {0};
    union {
        struct cmsghdr cm;
        char control[CMSG_SPACE(sizeof(int))];
    } control_un;

    iov.iov_base = data;
    iov.iov_len = *data_len;

    msg.msg_iov = &iov;
    msg.msg_iovlen = 1;
    msg.msg_control = control_un.control;
    msg.msg_controllen = sizeof(control_un.control);

    ssize_t n = recvmsg(unix_fd, &msg, MSG_CMSG_CLOEXEC);
    if (n < 0) {
        return -1;
    }

    *data_len = (size_t)n;

    /* Check for truncation */
    if (msg.msg_flags & MSG_TRUNC) {
        errno = EMSGSIZE;
        return -1;
    }
    if (msg.msg_flags & MSG_CTRUNC) {
        errno = EMSGSIZE;
        return -1;
    }

    /* Extract fd from control message */
    struct cmsghdr *cmsg = CMSG_FIRSTHDR(&msg);
    if (!cmsg || cmsg->cmsg_level != SOL_SOCKET || cmsg->cmsg_type != SCM_RIGHTS) {
        errno = EPROTO;
        return -1;
    }

    memcpy(client_fd, CMSG_DATA(cmsg), sizeof(*client_fd));
    return 0;
}

int handoff_validate_socket(int fd) {
    int type;
    socklen_t len = sizeof(type);
    if (getsockopt(fd, SOL_SOCKET, SO_TYPE, &type, &len) < 0) {
        return -1;
    }
    if (type != SOCK_STREAM) {
        errno = EPROTOTYPE;
        return -1;
    }
    return 0;
}

int handoff_parse_data(const char *data, size_t len, handoff_data_t *out) {
    memset(out, 0, sizeof(*out));

    /* Skip leading whitespace and NUL bytes */
    while (len > 0 && (data[0] == '\0' || data[0] == ' ' ||
                       data[0] == '\t' || data[0] == '\n' || data[0] == '\r')) {
        data++;
        len--;
    }

    /* Trim trailing whitespace and NUL bytes */
    while (len > 0 && (data[len-1] == '\0' || data[len-1] == ' ' ||
                       data[len-1] == '\t' || data[len-1] == '\n' || data[len-1] == '\r')) {
        len--;
    }

    /* Empty data is valid - just means no handoff data */
    if (len == 0) {
        return 0;
    }

    /* Parse JSON */
    jsmntok_t tokens[MAX_JSON_TOKENS];
    int num_tokens = json_parse(data, len, tokens, MAX_JSON_TOKENS);
    if (num_tokens < 0) {
        /* Failed to parse - not an error, just use defaults */
        return 0;
    }

    /* Extract user_id */
    int idx = json_find_key(data, tokens, num_tokens, "user_id");
    if (idx > 0) {
        out->user_id = json_tok_int(data, &tokens[idx]);
    }

    /* Extract max_tokens */
    idx = json_find_key(data, tokens, num_tokens, "max_tokens");
    if (idx > 0) {
        out->max_tokens = (int)json_tok_int(data, &tokens[idx]);
    }

    /* Extract prompt - point directly into the data buffer
     * This works because handoff_buf is stable for the connection lifetime
     */
    idx = json_find_key(data, tokens, num_tokens, "prompt");
    if (idx > 0 && tokens[idx].type == JSMN_STRING) {
        out->prompt = (char *)data + tokens[idx].start;
        /* Null-terminate in place (modifying the buffer) */
        ((char *)data)[tokens[idx].end] = '\0';
    }

    /* Extract model */
    idx = json_find_key(data, tokens, num_tokens, "model");
    if (idx > 0 && tokens[idx].type == JSMN_STRING) {
        out->model = (char *)data + tokens[idx].start;
        ((char *)data)[tokens[idx].end] = '\0';
    }

    return 0;
}
