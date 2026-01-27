#!/bin/bash
#
# Example handler script for fdrecv
#
# Receives:
#   - stdin/stdout connected to client socket
#   - HANDOFF_DATA environment variable with JSON data
#
# Must output a complete HTTP response (headers + body)

# Parse JSON data (simple extraction, use jq for real parsing)
USER_ID=$(echo "$HANDOFF_DATA" | grep -o '"user_id":[0-9]*' | cut -d: -f2)
PROMPT=$(echo "$HANDOFF_DATA" | grep -o '"prompt":"[^"]*"' | cut -d: -f2 | tr -d '"')

# Send HTTP headers
echo -en "HTTP/1.1 200 OK\r\n"
echo -en "Content-Type: text/event-stream\r\n"
echo -en "Cache-Control: no-cache\r\n"
echo -en "Connection: close\r\n"
echo -en "\r\n"

# Stream SSE response
echo "data: {\"content\": \"Hello from bash handler!\", \"index\": 0}"
echo
sleep 0.2

echo "data: {\"content\": \"User ID: $USER_ID\", \"index\": 1}"
echo
sleep 0.2

echo "data: {\"content\": \"Prompt: $PROMPT\", \"index\": 2}"
echo
sleep 0.2

echo "data: {\"content\": \"This response is coming from a shell script.\", \"index\": 3}"
echo
sleep 0.2

echo "data: {\"content\": \"The fdrecv daemon received the socket via SCM_RIGHTS.\", \"index\": 4}"
echo
sleep 0.2

echo "data: {\"content\": \"Then it forked and exec'd this script with stdin/stdout as the client socket.\", \"index\": 5}"
echo
sleep 0.2

echo "data: [DONE]"
echo
