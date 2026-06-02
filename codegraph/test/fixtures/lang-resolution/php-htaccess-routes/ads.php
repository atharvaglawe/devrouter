<?php
$slot = $_GET['slot'] ?? 'default';
header('Content-Type: application/json');
echo json_encode(['slot' => $slot, 'ads' => []]);
