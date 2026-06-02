<?php
// Outbound HTTP idioms the plain-PHP extractor must recognise.

function fetchProfile(): string
{
    return file_get_contents('http://internal.example.com/profile');
}

function streamLogs(): void
{
    $fh = fopen('http://logs.example.com/tail', 'r');
    if ($fh) {
        fclose($fh);
    }
}

function deleteRecord(): bool
{
    $ch = curl_init();
    curl_setopt($ch, CURLOPT_URL, 'http://api.example.com/records');
    curl_setopt($ch, CURLOPT_CUSTOMREQUEST, 'DELETE');
    curl_setopt($ch, CURLOPT_RETURNTRANSFER, true);
    $resp = curl_exec($ch);
    curl_close($ch);
    return $resp !== false;
}

function postEvent(array $event): void
{
    $ch = curl_init();
    curl_setopt($ch, CURLOPT_URL, 'http://events.example.com/ingest');
    curl_setopt($ch, CURLOPT_POST, true);
    curl_setopt($ch, CURLOPT_POSTFIELDS, json_encode($event));
    curl_exec($ch);
    curl_close($ch);
}
