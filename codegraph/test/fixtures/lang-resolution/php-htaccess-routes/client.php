<?php
// Outbound HTTP call into one of our own htaccess-registered routes.
// The pipeline should join this FETCHES to the /healthz Route.

function checkHealth(): bool
{
    $body = file_get_contents('http://service.local/healthz');
    return $body !== false;
}
