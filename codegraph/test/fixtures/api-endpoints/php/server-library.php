<?php
// Pure library: only function/class definitions, no top-level
// executable code. Should NOT be reported as a route.

namespace App\Library;

class StringUtils
{
    public static function snake(string $s): string
    {
        return strtolower(preg_replace('/[A-Z]/', '_$0', $s));
    }
}

function joinPath(string $a, string $b): string
{
    return rtrim($a, '/') . '/' . ltrim($b, '/');
}
