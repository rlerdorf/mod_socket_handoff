/*
 * handoff.h - SCM_RIGHTS file descriptor receiving
 */
#ifndef HANDOFF_H
#define HANDOFF_H

#include "daemon.h"
#include <sys/socket.h>

/* Receive file descriptor and data via SCM_RIGHTS
 * This is a blocking call - use with recvmsg or io_uring IORING_OP_RECVMSG
 * Returns 0 on success, -1 on error
 */
int handoff_recv_fd(int unix_fd, int *client_fd,
                    char *data, size_t *data_len);

/* Parse handoff JSON data
 * Populates fields in handoff_data_t, pointing into the data buffer
 * Returns 0 on success, -1 on error
 */
int handoff_parse_data(const char *data, size_t len, handoff_data_t *out);

/* Validate received socket is a stream socket */
int handoff_validate_socket(int fd);

/* Prepare msghdr for recvmsg (for io_uring) */
void handoff_prepare_msghdr(struct msghdr *msg, struct iovec *iov,
                            char *data_buf, size_t data_buf_len,
                            char *ctrl_buf, size_t ctrl_buf_len);

/* Extract fd from received control message */
int handoff_extract_fd(struct msghdr *msg);

#endif /* HANDOFF_H */
