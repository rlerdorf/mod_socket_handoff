<?php
/**
 * Test script for mod_socket_handoff
 *
 * Deploy to web root and access via browser or curl:
 *   curl -N http://localhost/test.php?prompt=Hello
 *
 * Expected: SSE stream from the streaming daemon
 */

declare(strict_types=1);

// Simulate authentication
$user_id = 12345;

// Get prompt from request
$prompt = $_GET['prompt'] ?? $_POST['prompt'] ?? 'Default test prompt';

// Prepare handoff data
$handoff_data = json_encode([
    'user_id' => $user_id,
    'prompt' => $prompt,
    'model' => 'test-model',
    'timestamp' => time(),
], JSON_THROW_ON_ERROR);

// Set headers for handoff
// The module will intercept these and pass the connection to the daemon
header('X-Socket-Handoff: /var/run/streaming-daemon.sock');
header('X-Handoff-Data: ' . $handoff_data);

// Exit immediately
// The module takes over from here - Apache worker is freed
exit;
