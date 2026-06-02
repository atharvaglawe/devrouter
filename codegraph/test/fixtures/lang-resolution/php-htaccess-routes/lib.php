<?php
// Pure library — should produce no Route node.

namespace App\Lib;

class Formatter
{
    public static function dollars(int $cents): string
    {
        return '$' . number_format($cents / 100, 2);
    }
}

function pluralize(string $word, int $n): string
{
    return $n === 1 ? $word : $word . 's';
}
