<?php
// Handler that narrows method by checking REQUEST_METHOD.

if ($_SERVER['REQUEST_METHOD'] === 'POST') {
    $payload = json_decode(file_get_contents('php://input'), true);
    header('Content-Type: application/json');
    echo json_encode(['received' => $payload]);
    return;
}

http_response_code(405);
echo 'Method Not Allowed';
