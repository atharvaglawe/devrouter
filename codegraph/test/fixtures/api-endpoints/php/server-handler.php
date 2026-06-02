<?php
// Plain-PHP request handler. Reads $_GET, sets a header, echoes JSON.
// extractPhpApiEndpoints should emit a `php.fileBased` route here.

$id = $_GET['id'] ?? null;
if (!$id) {
    http_response_code(400);
    echo json_encode(['error' => 'missing id']);
    return;
}

header('Content-Type: application/json');
echo json_encode(['id' => $id, 'ok' => true]);
