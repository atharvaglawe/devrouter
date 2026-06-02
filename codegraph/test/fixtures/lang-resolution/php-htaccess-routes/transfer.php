<?php
$amount = $_GET['amount'] ?? 0;
header('Content-Type: application/json');
echo json_encode(['amount' => (int) $amount]);
